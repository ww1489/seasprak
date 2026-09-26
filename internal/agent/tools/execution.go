package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

const approvalUnavailableMessage = "execution approval is not available"

type Outcome struct {
	Status     string
	Content    string
	SideEffect string
	Executed   bool
}

type Executor struct {
	defs         map[string]Definition
	comp         map[string]*jsonschema.Schema
	sink         agent.ExecutionSink
	auth         agent.ToolAuthorizer
	budg         *agent.BudgetLedger
	gen          string
	operations   Operations
	scheduler    *ResourceScheduler
	environment  string
	workspace    string
	standaloneID string
	tickets      *agent.ExecutionTickets
}

func NewExecutor(gen string, defs []Definition, sink agent.ExecutionSink, auth agent.ToolAuthorizer, budg *agent.BudgetLedger, options ...ExecutorOption) (*Executor, error) {
	tickets, err := agent.NewExecutionTickets()
	if err != nil {
		return nil, err
	}
	standaloneID, err := agent.NewID()
	if err != nil {
		return nil, err
	}
	e := &Executor{defs: map[string]Definition{}, comp: map[string]*jsonschema.Schema{}, sink: sink, auth: auth, budg: budg, gen: gen, scheduler: SharedResourceScheduler(), standaloneID: "standalone:" + standaloneID, tickets: tickets}
	for _, option := range options {
		option(e)
	}
	if len(e.comp) == 0 && len(defs) != 0 {
		e.comp, err = CompileSchemas(defs)
		if err != nil {
			return nil, err
		}
	}
	if len(e.comp) != len(defs) {
		return nil, product.NewError(product.CodeInvalidArgument, "compiled schemas do not match tool definitions")
	}
	for _, def := range defs {
		if def.Run != nil && def.RunWithOutput != nil {
			return nil, product.NewError(product.CodeInvalidArgument, "tool has conflicting execution callbacks")
		}
		if def.Name == "" || e.comp[def.Name] == nil {
			return nil, product.NewError(product.CodeInvalidArgument, "compiled schema is missing")
		}
		if _, ok := e.defs[def.Name]; ok {
			return nil, product.NewError(product.CodeInvalidArgument, "duplicate tool name")
		}
		e.defs[def.Name] = def.Clone()
	}
	return e, nil
}

func (e *Executor) Run(ctx context.Context, scope agent.ExecutionScope, callID, name, arguments string) (Outcome, error) {
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	accepted, record, err := e.lookup(ctx, scope, callID)
	if err != nil {
		return Outcome{}, err
	}
	if accepted && record.Observation != nil {
		if record.Observation.Status == "denied" && record.Observation.Content == approvalUnavailableMessage {
			return Outcome{}, product.NewError(product.CodeResourceUnavailable, approvalUnavailableMessage)
		}
		return outcomeOf(record.Observation), nil
	}
	if accepted && record.Claimed {
		return Outcome{}, product.NewError(product.CodeStateConflict, "claimed tool call has no observation")
	}
	envelope := scope
	if accepted {
		scope, envelope.TurnID = record.Scope, record.Scope.TurnID
	}
	call := e.freeze(callID, name, arguments, "")
	if accepted {
		call = record.Call
		if call.Name != name || call.ProviderCallID != callID || call.Arguments != arguments {
			return e.saveObservation(ctx, envelope, scope, call, Outcome{Status: "failed", Content: "tool call does not match the accepted call", SideEffect: "none"}, false, true)
		}
	}
	def, known := e.defs[name]
	if !known {
		return e.reject(ctx, envelope, scope, call, Outcome{Status: "denied", Content: "unknown tool", SideEffect: "none"}, accepted)
	}
	if !accepted {
		call = e.freeze(callID, name, arguments, def.Version)
	}
	hookCtx, finishHooks := context.WithTimeout(ctx, e.budg.Limits().HookTimeout)
	defer finishHooks()
	final, err := e.prepareArguments(hookCtx, def, arguments)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return e.rejectWithError(ctx, envelope, scope, call, Outcome{Status: "cancelled", Content: err.Error(), SideEffect: "none"}, accepted, err)
		}
		return e.reject(ctx, envelope, scope, call, Outcome{Status: "failed", Content: "invalid tool arguments", SideEffect: "none"}, accepted)
	}
	description, err := resolveDescription(hookCtx, def, final)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return e.rejectWithError(ctx, envelope, scope, call, Outcome{Status: "cancelled", Content: err.Error(), SideEffect: "none"}, accepted, err)
		}
		return e.rejectWithError(ctx, envelope, scope, call, Outcome{Status: "failed", Content: err.Error(), SideEffect: "none"}, accepted, err)
	}
	if err := hookCtx.Err(); err != nil {
		return e.rejectWithError(ctx, envelope, scope, call, Outcome{Status: "cancelled", Content: err.Error(), SideEffect: "none"}, accepted, err)
	}
	policyRef := "trusted-injected"
	if source, ok := e.sink.(agent.ExecutionPolicySource); ok {
		policyRef, err = source.ExecutionPolicyRef(ctx, envelope)
		if err != nil {
			return Outcome{}, err
		}
	}
	actualTimeout := e.budg.Limits().ToolTimeout
	if description.Timeout > 0 && description.Timeout < actualTimeout {
		actualTimeout = description.Timeout
	}
	frozen := agent.FrozenExecution{
		ID: "execution:" + call.CallID, CallID: call.CallID, Scope: scope, Origin: "model", Tool: name,
		ToolVersion: def.Version, SchemaHash: argumentHash(def.Schema), Generation: call.Generation,
		ProviderCallID: call.ProviderCallID, OriginalArgumentsHash: argumentHash([]byte(call.Arguments)),
		FinalArgumentsHash: argumentHash(final), ArgumentsRef: "arguments:" + call.CallID,
		FinalArguments: append(json.RawMessage(nil), final...), Resources: description.Resources,
		Effect: description.Effect, Concurrency: description.Concurrency, BackendID: description.BackendID,
		Argv: description.Argv, Cwd: description.Cwd, EnvironmentRef: description.EnvironmentRef,
		StdinRef: description.StdinRef, Mounts: description.Mounts, TempRootRef: description.TempRootRef,
		OutputLimitBytes: description.OutputLimitBytes, Timeout: actualTimeout, PolicyRef: policyRef, RequestedGrantRef: description.RequestedGrantRef,
	}
	if !accepted {
		frozen.Scope = envelope
	}
	frozen.Hash, err = frozen.Digest()
	if err != nil {
		return Outcome{}, err
	}
	// Standalone legacy test sinks have no policy/state port. Their simple
	// trusted Run path keeps its historical fact sequence; production always
	// commits the descriptor before hooks and authorization.
	_, sessionSource := e.sink.(agent.ExecutionPolicySource)
	commitFrozen := sessionSource || len(def.PrepareArguments) != 0 || def.ResolveExecution != nil || len(def.BeforeCall) != 0
	if commitFrozen {
		if err := e.commitFrozen(ctx, envelope, frozen); err != nil {
			return Outcome{}, err
		}
	}
	if err := runBeforeHooks(hookCtx, def.BeforeCall, frozen); err != nil {
		return e.rejectWithError(ctx, envelope, scope, call, deniedOutcome(err), accepted, err)
	}
	authorization := call
	authorization.Arguments, authorization.Hash = string(final), frozen.Hash
	decision, authErr := e.auth.Authorize(ctx, authorization)
	if !commitFrozen && (authErr != nil || decision != agent.DecisionAllow) {
		if err := e.commitFrozen(ctx, envelope, frozen); err != nil {
			return Outcome{}, err
		}
	}
	if decision == agent.DecisionAsk {
		unavailable := product.NewError(product.CodeResourceUnavailable, approvalUnavailableMessage)
		out, saveErr := e.reject(ctx, envelope, scope, call, Outcome{Status: "denied", Content: approvalUnavailableMessage, SideEffect: "none"}, accepted)
		if saveErr != nil {
			return out, errors.Join(unavailable, authErr, saveErr)
		}
		if authErr != nil {
			return out, authErr
		}
		return out, unavailable
	}
	if authErr != nil {
		return e.rejectWithError(ctx, envelope, scope, call, deniedOutcome(authErr), accepted, authErr)
	}
	if decision != agent.DecisionAllow {
		status := "denied"
		if decision == agent.DecisionCancel {
			status = "cancelled"
		}
		return e.reject(ctx, envelope, scope, call, Outcome{Status: status, Content: "execution is not allowed", SideEffect: "none"}, accepted)
	}
	if err := ctx.Err(); err != nil {
		return e.rejectWithError(ctx, envelope, scope, call, Outcome{Status: "cancelled", Content: err.Error(), SideEffect: "none"}, accepted, err)
	}
	if frozen.BackendID == "trusted-run" && def.Run == nil && def.RunWithOutput == nil {
		return e.reject(ctx, envelope, scope, call, Outcome{Status: "failed", Content: "tool runner is nil", SideEffect: "none"}, accepted)
	}
	if err := e.backendAvailable(def, frozen); err != nil {
		return e.rejectWithError(ctx, envelope, scope, call, deniedOutcome(err), accepted, err)
	}
	lease, err := e.acquire(ctx, envelope, frozen)
	if err != nil {
		return Outcome{}, err
	}
	retain := false
	defer func() {
		if retain {
			owner := envelope.SessionID
			if owner == "" {
				owner = e.standaloneID
			}
			_ = lease.Retain(ResourceHoldID(owner, call.CallID))
		} else {
			lease.Release()
		}
	}()
	if err := ctx.Err(); err != nil {
		return e.rejectWithError(ctx, envelope, scope, call, Outcome{Status: "cancelled", Content: err.Error(), SideEffect: "none"}, accepted, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, frozen.Timeout)
	defer cancel()
	deadline, _ := runCtx.Deadline()
	receipt, err := e.budg.ClaimTool(runCtx, e.sink, envelope, call, frozen)
	if err != nil {
		if isBudgetOrCancel(err) {
			out := deniedOutcome(err)
			if pe, ok := product.AsError(err); ok && pe.Code == product.CodeBudgetExhausted {
				out.Status = "failed"
			}
			return e.rejectWithError(ctx, envelope, scope, call, out, accepted, err)
		}
		return Outcome{}, err
	}
	var validator agent.ExecutionTicketValidator
	if v, ok := e.sink.(agent.ExecutionTicketValidator); ok {
		validator = v
	}
	ticket, err := e.tickets.Issue(receipt, frozen, deadline, validator)
	if err != nil {
		retain = true // A durable claim without a start observation is unresolved.
		return Outcome{}, err
	}
	var out Outcome
	if err := runCtx.Err(); err != nil {
		out = Outcome{Status: "cancelled", Content: err.Error(), SideEffect: "none"}
		if _, saveErr := e.saveObservation(ctx, envelope, scope, call, out, true, true); saveErr != nil {
			return out, errors.Join(err, saveErr)
		}
		return out, err
	}
	output := &callOutput{runCtx: runCtx, sink: e.sink, scope: envelope, callID: call.CallID, streamID: agent.MustID()}
	out = e.invokeAuthorized(runCtx, def, frozen, ticket, output)
	output.close() // Drain accepted publications before saving the final observation.
	if out.SideEffect == "unknown" || out.Status == "outcome_unknown" {
		retain = true
	}
	if _, saveErr := e.saveObservation(ctx, envelope, scope, call, out, true, true); saveErr != nil {
		if out.Executed || out.SideEffect != "none" || out.Status != "cancelled" {
			retain = true
		}
		return out, saveErr
	}
	return out, nil
}

func (e *Executor) backendAvailable(def Definition, frozen agent.FrozenExecution) error {
	switch frozen.BackendID {
	case "trusted-run":
		if (def.Run != nil || def.RunWithOutput != nil) && len(frozen.Argv) == 0 && frozen.Cwd == "" && len(frozen.Mounts) == 0 && frozen.StdinRef == "" && frozen.TempRootRef == "" {
			return nil
		}
	case "process-operations":
		if e.operations.Process != nil && len(frozen.Argv) > 0 {
			return nil
		}
	case "file-operations":
		if e.operations.Files != nil && len(frozen.Argv) == 0 {
			return nil
		}
	case "artifact-store":
		if e.operations.Artifacts != nil {
			return nil
		}
	}
	return product.NewError(product.CodeResourceUnavailable, "controlled execution backend is unavailable")
}

func (e *Executor) acquire(ctx context.Context, scope agent.ExecutionScope, frozen agent.FrozenExecution) (*ResourceLease, error) {
	environment, workspace := e.environment, e.workspace
	if environment == "" {
		environment = "trusted-injected"
	}
	if workspace == "" {
		workspace = scope.SessionID
		if workspace == "" {
			// An executor-lifetime identity outlives allocator address reuse and
			// keeps historical unknown holds separate from unrelated executors.
			workspace = e.standaloneID
		}
	}
	if e.scheduler == nil {
		return nil, product.NewError(product.CodeResourceUnavailable, "resource scheduler is unavailable")
	}
	return e.scheduler.Acquire(ctx, ResourceRequest{Environment: environment, Workspace: workspace, Resources: frozen.Resources, Effect: frozen.Effect, Concurrency: frozen.Concurrency})
}

func (e *Executor) commitFrozen(ctx context.Context, envelope agent.ExecutionScope, frozen agent.FrozenExecution) error {
	raw, err := json.Marshal(frozen)
	if err != nil {
		return err
	}
	return e.sink.CommitFact(ctx, envelope, agent.Fact{Kind: "tool_frozen", Payload: raw})
}

func deniedOutcome(err error) Outcome {
	status := "denied"
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = "cancelled"
	}
	return Outcome{Status: status, Content: err.Error(), SideEffect: "none"}
}
func isBudgetOrCancel(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	pe, ok := product.AsError(err)
	return ok && (pe.Code == product.CodeBudgetExhausted || pe.Code == product.CodePermissionDenied || pe.Code == product.CodeStateConflict || pe.Code == product.CodeResourceUnavailable)
}

func (e *Executor) lookup(ctx context.Context, scope agent.ExecutionScope, providerID string) (bool, agent.ToolRecord, error) {
	src, ok := e.sink.(agent.ToolCallSource)
	if !ok {
		return false, agent.ToolRecord{}, nil
	}
	rec, err := src.LookupTool(ctx, scope, providerID)
	if err != nil {
		if pe, ok := product.AsError(err); ok && pe.Code == product.CodeNotFound {
			return false, agent.ToolRecord{}, nil
		}
		return false, agent.ToolRecord{}, err
	}
	if rec.Call.CallID == "" && rec.Call.ProviderCallID == "" && rec.Observation == nil && !rec.Claimed {
		return false, rec, nil
	}
	return true, rec, nil
}

func (e *Executor) RejectUnavailable(ctx context.Context, scope agent.ExecutionScope, callID, name, arguments string) (Outcome, error) {
	accepted, rec, err := e.lookup(ctx, scope, callID)
	if err != nil {
		return Outcome{}, err
	}
	if !accepted {
		return Outcome{Status: "denied", Content: "tool is unavailable", SideEffect: "none"}, nil
	}
	if rec.Call.ProviderCallID != callID || rec.Call.Name != name || rec.Call.Arguments != arguments {
		return Outcome{}, product.NewError(product.CodeStateConflict, "unavailable tool does not match accepted call")
	}
	if rec.Observation != nil {
		return outcomeOf(rec.Observation), nil
	}
	return e.saveObservation(ctx, scope, rec.Scope, rec.Call, Outcome{Status: "denied", Content: "tool is unavailable", SideEffect: "none"}, false, true)
}

func (e *Executor) reject(ctx context.Context, envelope, scope agent.ExecutionScope, call agent.FrozenCall, out Outcome, accepted bool) (Outcome, error) {
	return e.saveObservation(ctx, envelope, scope, call, out, false, accepted)
}
func (e *Executor) rejectWithError(ctx context.Context, envelope, scope agent.ExecutionScope, call agent.FrozenCall, out Outcome, accepted bool, cause error) (Outcome, error) {
	out, err := e.reject(ctx, envelope, scope, call, out, accepted)
	if err != nil {
		return out, errors.Join(cause, err)
	}
	return out, cause
}
func (e *Executor) saveObservation(ctx context.Context, envelope, scope agent.ExecutionScope, call agent.FrozenCall, out Outcome, claimed, write bool) (Outcome, error) {
	if !write {
		return out, nil
	}
	record := agent.ToolRecord{Call: call, Scope: scope, Claimed: claimed, Observation: &agent.ToolObservation{Status: out.Status, Content: out.Content, SideEffect: out.SideEffect, Executed: out.Executed}}
	body, err := json.Marshal(record)
	if err != nil {
		return out, err
	}
	return out, e.sink.CommitFact(context.WithoutCancel(ctx), envelope, agent.Fact{Kind: "tool_observation", Payload: body})
}

func (e *Executor) freeze(providerID, name, arguments, version string) agent.FrozenCall {
	return agent.FrozenCall{CallID: providerID, ProviderCallID: providerID, Name: name, Arguments: arguments, Generation: e.gen, Hash: callHash(name, version, arguments, e.gen)}
}
func callHash(name, version, arguments, gen string) string {
	sum := sha256.Sum256([]byte(name + "\n" + version + "\n" + arguments + "\n" + gen))
	return hex.EncodeToString(sum[:])
}
func outcomeOf(obs *agent.ToolObservation) Outcome {
	if obs == nil {
		return Outcome{}
	}
	return Outcome{Status: obs.Status, Content: obs.Content, SideEffect: obs.SideEffect, Executed: obs.Executed}
}
func DecodeNumbers(r io.Reader) (any, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

func (e *Executor) invokeAuthorized(ctx context.Context, def Definition, frozen agent.FrozenExecution, ticket agent.ExecutionTicketRef, output agent.ToolOutputSink) (out Outcome) {
	out = Outcome{Status: "succeeded", SideEffect: "none"}
	defer func() {
		if value := recover(); value != nil {
			out = Outcome{Status: "failed", Content: "controlled execution panicked", SideEffect: "unknown", Executed: true}
		}
	}()
	auth := agent.NewAuthorizedExecution(ticket, frozen)
	switch frozen.BackendID {
	case "trusted-run":
		if err := auth.Validate(ctx); err != nil {
			return deniedOutcome(err)
		}
		if err := ctx.Err(); err != nil {
			return deniedOutcome(err)
		}
		var content string
		var err error
		if def.RunWithOutput != nil {
			content, err = def.RunWithOutput(ctx, append(json.RawMessage(nil), frozen.FinalArguments...), output)
		} else {
			content, err = def.Run(ctx, append(json.RawMessage(nil), frozen.FinalArguments...))
		}
		out.Content, out.Executed = content, true
		if err != nil {
			out.Status, out.Content, out.SideEffect = "failed", "trusted tool execution failed", "unknown"
		}
	case "process-operations":
		observation, err := e.operations.Process.Execute(ctx, agent.AuthorizedProcess{Authorization: auth,
			Argv: append([]string(nil), frozen.Argv...), Cwd: frozen.Cwd, EnvironmentRef: frozen.EnvironmentRef,
			StdinRef: frozen.StdinRef, Mounts: append([]agent.ExecutionMount(nil), frozen.Mounts...),
			TempRootRef: frozen.TempRootRef, OutputLimitBytes: frozen.OutputLimitBytes}, processOutput{output: output})
		out.Content, out.Executed, out.SideEffect = observation.Content, observation.Started, observation.SideEffect
		if out.SideEffect == "" {
			out.SideEffect = "none"
			if err != nil || !observation.Terminated {
				out.SideEffect = "unknown"
			}
		}
		if err != nil {
			out.Status, out.Content = "failed", "controlled process execution failed"
			if pe, ok := product.AsError(err); ok {
				out.Content = pe.Code
			}
		}
		// A nonterminal process may still produce effects even if none have
		// been observed so far. Keep confirmed effects, but retain the hold
		// until termination is proved; a return from Execute is not that proof.
		if !observation.Terminated && (observation.Started || (err == nil && out.SideEffect != "none")) {
			out.Status = "outcome_unknown"
		}
	case "file-operations":
		out = e.invokeFile(ctx, auth, frozen)
	case "artifact-store":
		out = e.invokeArtifact(ctx, auth, frozen)
	default:
		return deniedOutcome(product.NewError(product.CodeResourceUnavailable, "controlled execution backend is unavailable"))
	}
	if out.SideEffect == "unknown" && !out.Executed && out.Status == "succeeded" {
		out.Status = "outcome_unknown"
	}
	return out
}

func (e *Executor) invokeFile(ctx context.Context, auth agent.AuthorizedExecution, frozen agent.FrozenExecution) Outcome {
	var command struct {
		Operation, Path, ContentRef, PatchRef, ExpectedVersion string
	}
	if err := json.Unmarshal(frozen.FinalArguments, &command); err != nil {
		return Outcome{Status: "failed", Content: "file request is invalid", SideEffect: "none"}
	}
	var effect agent.FileEffect
	var err error
	switch command.Operation {
	case "write":
		effect, err = e.operations.Files.Write(ctx, agent.AuthorizedFileWrite{Authorization: auth, Path: command.Path, ContentRef: command.ContentRef, ExpectedVersion: command.ExpectedVersion})
	case "edit":
		effect, err = e.operations.Files.Edit(ctx, agent.AuthorizedFileEdit{Authorization: auth, Path: command.Path, PatchRef: command.PatchRef, ExpectedVersion: command.ExpectedVersion})
	default:
		return Outcome{Status: "failed", Content: "file operation is unavailable", SideEffect: "none"}
	}
	out := Outcome{Status: "succeeded", Content: effect.Identity, Executed: effect.Confirmed, SideEffect: effect.SideEffect}
	if out.SideEffect == "" {
		out.SideEffect = "unknown"
	}
	if err != nil {
		out.Status, out.Content = "failed", "controlled file operation failed"
	}
	if out.SideEffect == "unknown" && !out.Executed {
		out.Status = "outcome_unknown"
	}
	return out
}
func (e *Executor) invokeArtifact(ctx context.Context, auth agent.AuthorizedExecution, frozen agent.FrozenExecution) Outcome {
	var input struct{ Operation, ContentRef, MediaType, Name string }
	if err := json.Unmarshal(frozen.FinalArguments, &input); err != nil || input.Operation != "save" {
		return Outcome{Status: "failed", Content: "artifact request is invalid", SideEffect: "none"}
	}
	ref, err := e.operations.Artifacts.Save(ctx, agent.ArtifactInput{Authorization: auth, ContentRef: input.ContentRef, MediaType: input.MediaType, Name: input.Name})
	if err != nil {
		return Outcome{Status: "failed", Content: "controlled artifact save failed", SideEffect: "unknown", Executed: true}
	}
	return Outcome{Status: "succeeded", Content: ref.ID, SideEffect: "none", Executed: true}
}
