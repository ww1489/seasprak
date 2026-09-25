package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	product "github.com/ww1489/seasprak/internal/errors"
	"io"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ww1489/seasprak/internal/agent"
)

type Definition struct {
	Version     string
	Name        string
	Description string
	Schema      json.RawMessage
	Run         func(ctx context.Context, args json.RawMessage) (string, error)
}

type Outcome struct {
	Status     string
	Content    string
	SideEffect string
	Executed   bool
}

type Executor struct {
	defs map[string]Definition
	comp map[string]*jsonschema.Schema
	sink agent.ExecutionSink
	auth agent.ToolAuthorizer
	budg *agent.BudgetLedger
	gen  string
}

func NewExecutor(gen string, defs []Definition, sink agent.ExecutionSink, auth agent.ToolAuthorizer, budg *agent.BudgetLedger) (*Executor, error) {
	e := &Executor{
		defs: map[string]Definition{},
		comp: map[string]*jsonschema.Schema{},
		sink: sink,
		auth: auth,
		budg: budg,
		gen:  gen,
	}
	compiler := jsonschema.NewCompiler()
	for _, def := range defs {
		if _, ok := e.defs[def.Name]; ok {
			return nil, product.Errorf(product.CodeInvalidArgument, "duplicate tool %s", def.Name)
		}
		var doc any
		if err := json.Unmarshal(def.Schema, &doc); err != nil {
			return nil, err
		}
		name := def.Name + ".schema.json"
		if err := compiler.AddResource("file:///"+name, doc); err != nil {
			return nil, err
		}
		sch, err := compiler.Compile("file:///" + name)
		if err != nil {
			return nil, err
		}
		e.defs[def.Name] = def
		e.comp[def.Name] = sch
	}
	return e, nil
}

func (e *Executor) Run(ctx context.Context, scope agent.ExecutionScope, callID, name, arguments string) (Outcome, error) {
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	accepted, rec, err := e.lookup(ctx, scope, callID)
	if err != nil {
		return Outcome{}, err
	}
	if accepted && rec.Observation != nil {
		return outcomeOf(rec.Observation), nil
	}
	if accepted && rec.Claimed {
		return Outcome{}, product.NewError(product.CodeStateConflict, "claimed tool call has no observation")
	}
	if accepted {
		scope = rec.Scope
	}
	frozen := e.freeze(callID, name, arguments, "")
	if accepted {
		if rec.Call.Name != name || rec.Call.Arguments != arguments {
			return e.saveObservation(ctx, scope, rec.Call, Outcome{Status: "failed", Content: "tool call does not match the accepted call", SideEffect: "none"}, false, true)
		}
		frozen = rec.Call
	}
	def, ok := e.defs[name]
	if !ok {
		return e.reject(ctx, scope, frozen, Outcome{Status: "denied", Content: "unknown tool", SideEffect: "none"}, accepted)
	}
	if !accepted {
		frozen = e.freeze(callID, name, arguments, def.Version)
	}
	raw := json.RawMessage(arguments)
	if !json.Valid(raw) {
		return e.reject(ctx, scope, frozen, Outcome{Status: "failed", Content: "arguments are not json", SideEffect: "none"}, accepted)
	}
	var decoded any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		return e.reject(ctx, scope, frozen, Outcome{Status: "failed", Content: err.Error(), SideEffect: "none"}, accepted)
	}
	if err := e.comp[name].Validate(decoded); err != nil {
		return e.reject(ctx, scope, frozen, Outcome{Status: "failed", Content: err.Error(), SideEffect: "none"}, accepted)
	}
	decision, err := e.auth.Authorize(ctx, frozen)
	if err != nil {
		return Outcome{}, err
	}
	if decision != agent.DecisionAllow {
		return e.reject(ctx, scope, frozen, Outcome{Status: "denied", Content: "execution is not allowed", SideEffect: "none"}, accepted)
	}
	intent, err := json.Marshal(frozen)
	if err != nil {
		return Outcome{}, err
	}
	if err := e.sink.CommitFact(ctx, scope, agent.Fact{Kind: "tool_intent", Payload: intent}); err != nil {
		return Outcome{}, err
	}
	if err := e.budg.OccupyTool(); err != nil {
		out := Outcome{Status: "failed", Content: err.Error(), SideEffect: "none"}
		if _, saveErr := e.saveObservation(ctx, scope, frozen, out, true, true); saveErr != nil {
			return out, errors.Join(err, saveErr)
		}
		return out, err
	}
	if err := ctx.Err(); err != nil {
		out := Outcome{Status: "cancelled", Content: err.Error(), SideEffect: "none"}
		if _, saveErr := e.saveObservation(ctx, scope, frozen, out, true, true); saveErr != nil {
			return out, errors.Join(err, saveErr)
		}
		return out, err
	}
	runCtx, cancel := context.WithTimeout(ctx, e.budg.Limits().ToolTimeout)
	defer cancel()
	content, executed, runErr := e.invoke(runCtx, def, raw)
	out := Outcome{Status: "succeeded", Content: content, SideEffect: "none", Executed: true}
	if runErr != nil {
		side := "unknown"
		if !executed {
			side = "none"
		}
		out = Outcome{Status: "failed", Content: runErr.Error(), SideEffect: side, Executed: executed}
	}
	return e.saveObservation(ctx, scope, frozen, out, true, true)
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

func (e *Executor) reject(ctx context.Context, scope agent.ExecutionScope, frozen agent.FrozenCall, out Outcome, accepted bool) (Outcome, error) {
	if !accepted {
		return out, nil
	}
	return e.saveObservation(ctx, scope, frozen, out, false, true)
}

func (e *Executor) saveObservation(ctx context.Context, scope agent.ExecutionScope, frozen agent.FrozenCall, out Outcome, claimed, write bool) (Outcome, error) {
	if !write {
		return out, nil
	}
	rec := agent.ToolRecord{
		Call: frozen, Scope: scope, Claimed: claimed,
		Observation: &agent.ToolObservation{Status: out.Status, Content: out.Content, SideEffect: out.SideEffect, Executed: out.Executed},
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return out, err
	}
	if err := e.sink.CommitFact(ctx, scope, agent.Fact{Kind: "tool_observation", Payload: body}); err != nil {
		return out, err
	}
	return out, nil
}

func (e *Executor) invoke(ctx context.Context, def Definition, raw json.RawMessage) (content string, executed bool, err error) {
	if def.Run == nil {
		return "", false, fmt.Errorf("tool runner is nil")
	}
	defer func() {
		if rec := recover(); rec != nil {
			content, executed, err = "", true, fmt.Errorf("tool panic: %v", rec)
		}
	}()
	content, err = def.Run(ctx, raw)
	executed = true
	return content, executed, err
}

func (e *Executor) freeze(providerID, name, arguments, version string) agent.FrozenCall {
	return agent.FrozenCall{
		CallID: providerID, ProviderCallID: providerID, Name: name, Arguments: arguments, Generation: e.gen,
		Hash: callHash(name, version, arguments, e.gen),
	}
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

// DecodeNumbers preserves large integers as json.Number instead of float64.
func DecodeNumbers(r io.Reader) (any, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}
