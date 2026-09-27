package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

type capturedFixtureTicket struct {
	auth  agent.AuthorizedExecution
	apply func(context.Context) (agent.FileEffect, error)
}
type fixtureTicketResult struct {
	effect agent.FileEffect
	err    error
}

// The proxy never issues or consumes authority itself. It captures the actual
// Executor request before Memory validates it and keeps the worker alive until
// the test releases it. Only confirmed Memory effects count as writes.
type lifecycleFixtureFiles struct {
	*fixture.Memory
	entered             chan capturedFixtureTicket
	release             chan struct{}
	attempted           chan fixtureTicketResult
	consumeAfterRelease bool
	calls               atomic.Int32
	writes              atomic.Int32
}

func (p *lifecycleFixtureFiles) Write(ctx context.Context, r agent.AuthorizedFileWrite) (agent.FileEffect, error) {
	return p.capture(ctx, capturedFixtureTicket{r.Authorization, func(ctx context.Context) (agent.FileEffect, error) { return p.Memory.Write(ctx, r) }})
}
func (p *lifecycleFixtureFiles) Edit(ctx context.Context, r agent.AuthorizedFileEdit) (agent.FileEffect, error) {
	return p.capture(ctx, capturedFixtureTicket{r.Authorization, func(ctx context.Context) (agent.FileEffect, error) { return p.Memory.Edit(ctx, r) }})
}
func (p *lifecycleFixtureFiles) capture(ctx context.Context, request capturedFixtureTicket) (agent.FileEffect, error) {
	p.calls.Add(1)
	p.entered <- request
	<-p.release
	if !p.consumeAfterRelease {
		return agent.FileEffect{SideEffect: "none"}, product.NewError(product.CodeResourceUnavailable, "fixture stopped before consumption")
	}
	// Use a live context so the Session validator, not the caller's cancelled
	// context, must reject the ticket while the real worker remains active.
	effect, err := request.apply(context.WithoutCancel(ctx))
	if effect.Confirmed {
		p.writes.Add(1)
	}
	p.attempted <- fixtureTicketResult{effect, err}
	return effect, err
}

func TestTestkitTicketSessionLifecycle(t *testing.T) {
	for _, kind := range []string{"invokable", "enhanced-invokable"} {
		for _, operation := range []string{"write", "edit"} {
			for _, boundary := range []string{"settled", "cancel-before-consume"} {
				t.Run(kind+"/"+operation+"/"+boundary, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					m := fixture.NewMemory()
					version := m.SeedFile("document", []byte("original"))
					files := &lifecycleFixtureFiles{Memory: m, entered: make(chan capturedFixtureTicket, 1), release: make(chan struct{}), attempted: make(chan fixtureTicketResult, 1), consumeAfterRelease: boundary == "cancel-before-consume"}
					args := map[string]any{"path": "document", "expectedVersion": version}
					name := operation + "_file"
					if operation == "write" {
						args["contentRef"] = m.RegisterContent([]byte("replacement"))
					} else {
						args["patchRef"] = m.RegisterPatch(fixture.Patch{Old: "original", New: "replacement"})
					}
					raw, err := json.Marshal(args)
					if err != nil {
						t.Fatal(err)
					}
					def := builtinDefinitionForSession(t, name)
					def.ToolInterface = kind
					model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: name, Arguments: string(raw)}}}, testkit.Step{Text: "finished"})
					s, err := CreateAgentSession(ctx, Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: model, Tools: []tools.Definition{def}, Operations: tools.Operations{Files: files, Artifacts: m}, ResourceScheduler: tools.NewResourceScheduler()})
					if err != nil {
						t.Fatal(err)
					}
					defer s.Close(context.Background())
					defer func() {
						select {
						case <-files.release:
						default:
							close(files.release)
						}
					}()
					receipt, err := s.SubmitInput(ctx, agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"change the document"}`)})
					if err != nil {
						t.Fatal(err)
					}
					var request capturedFixtureTicket
					select {
					case request = <-files.entered:
					case <-ctx.Done():
						t.Fatal("file proxy not reached")
					}
					frame := activityFrame(t, s)
					if !request.auth.Ticket.Issued() {
						t.Fatal("real Executor did not issue a ticket")
					}
					view := s.rt.manager.View()
					call := view.Calls[request.auth.Frozen.CallID]
					if !call.Claimed || call.Observation != nil || view.Traces[receipt.TraceID].Usage.ToolExecutions != 1 || frame.budget.Snapshot().ToolExecutions != 1 || m.Calls(operation) != 0 {
						t.Fatal("proxy did not capture an unconsumed claimed request")
					}
					wantState := "completed"
					wantModelCalls := 2
					if boundary == "cancel-before-consume" {
						wantState = "cancelled"
						wantModelCalls = 1
						stopped := make(chan error, 1)
						go func() { stopped <- s.Cancel(ctx, receipt.TraceID) }()
						select {
						case <-frame.ctx.Done():
						case <-ctx.Done():
							t.Fatal("cancel intent was not accepted")
						}
						trace := s.rt.manager.View().Traces[receipt.TraceID]
						if trace.State != "cancelling" || trace.ExecutionStopped || trace.Settled || files.writes.Load() != 0 {
							t.Fatalf("cancellation prematurely stopped worker: %+v", trace)
						}
						select {
						case err := <-stopped:
							t.Fatalf("Cancel returned before proxy exit: %v", err)
						default:
						}
						close(files.release)
						var result fixtureTicketResult
						select {
						case result = <-files.attempted:
						case <-ctx.Done():
							t.Fatal("cancelled ticket was not checked")
						}
						// checkToolPolicyState returns the active execution's cancellation.
						if !errors.Is(result.err, context.Canceled) || result.effect.Confirmed || result.effect.SideEffect != "none" {
							t.Fatalf("cancelled consume=%+v err=%v", result.effect, result.err)
						}
						select {
						case err := <-stopped:
							if err != nil {
								t.Fatal(err)
							}
						case <-ctx.Done():
							t.Fatal("Cancel did not wait for actual exit")
						}
					} else {
						close(files.release)
					}
					select {
					case <-frame.done:
					case <-ctx.Done():
						t.Fatal("session worker did not exit")
					}
					view = s.rt.manager.View()
					trace := view.Traces[receipt.TraceID]
					call = view.Calls[request.auth.Frozen.CallID]
					if trace.State != wantState || !trace.Settled || !trace.ExecutionStopped || !call.Claimed || call.Observation == nil || call.Observation.Executed || call.Observation.SideEffect != "none" || call.Observation.Status != "failed" || trace.Usage.ToolExecutions != 1 || frame.budget.Snapshot().ToolExecutions != 1 || files.calls.Load() != 1 || files.writes.Load() != 0 || model.Calls() != wantModelCalls {
						t.Fatalf("settled trace=%+v call=%+v proxy=%d writes=%d model=%d", trace, call, files.calls.Load(), files.writes.Load(), model.Calls())
					}
					data, current, ok := m.Snapshot("document")
					if !ok || string(data) != "original" || current != version {
						t.Fatal("pre-replay file changed")
					}
					sequence := view.LastSeq
					// The ticket has never been consumed. A live Session rejects it after
					// settlement with state_conflict (no active execution), not the generic
					// permission_denied used for forged or already-consumed authorities.
					effect, err := request.apply(ctx)
					requireSessionCode(t, err, product.CodeStateConflict)
					if effect.Confirmed || effect.SideEffect != "none" {
						t.Fatalf("settled replay produced effect=%+v", effect)
					}
					data, current, ok = m.Snapshot("document")
					wantAttempts := 1
					if boundary == "cancel-before-consume" {
						wantAttempts++
					}
					after := s.rt.manager.View()
					persisted := after.Calls[request.auth.Frozen.CallID]
					if !ok || string(data) != "original" || current != version || m.Calls(operation) != wantAttempts || files.writes.Load() != 0 || !persisted.Claimed || after.Traces[receipt.TraceID].Usage.ToolExecutions != 1 || frame.budget.Snapshot().ToolExecutions != 1 || after.LastSeq != sequence {
						t.Fatal("replay changed file, claim, budget or journal")
					}
				})
			}
		}
	}
}
