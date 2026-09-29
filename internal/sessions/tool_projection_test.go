package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/sessions/store/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

type sessionLogProcess struct{ *fixture.Process }

func (p sessionLogProcess) Execute(ctx context.Context, in agent.AuthorizedProcess, sink agent.ProgressSink) (agent.ProcessObservation, error) {
	out, err := p.Process.Execute(ctx, in, sink)
	if out.Started {
		out.SideEffect = "confirmed"
	}
	return out, err
}

type sessionLogArtifacts struct {
	*fixture.Memory
	fail   bool
	saves  atomic.Int32
	before func()
	input  agent.OutputArtifactInput
}

func (s *sessionLogArtifacts) SaveOutput(ctx context.Context, in agent.OutputArtifactInput) (agent.ArtifactRef, error) {
	s.saves.Add(1)
	s.before()
	s.input = in
	if s.fail {
		return agent.ArtifactRef{}, errors.New("untrusted storage diagnostics")
	}
	return s.Memory.SaveOutput(ctx, in)
}

func TestProcessLogSessionProjectionSurvivesDiskReopen(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "saved"
		if fail {
			name = "save-failed"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			close(release)
			full := "HEAD\nfixture-private-value\n" + strings.Repeat("中文🚀\n", 2100) + "TAIL"
			process := sessionLogProcess{fixture.NewProcess(release, full)}
			artifacts := &sessionLogArtifacts{Memory: fixture.NewMemory(), fail: fail}
			gate := make(chan struct{})
			defer func() {
				select {
				case <-gate:
				default:
					close(gate)
				}
			}()
			model := versionedPauseModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider-log", Name: "execute", Arguments: `{"argv":["fixture-command"]}`}}}, testkit.Step{Text: "done", Gate: gate})}
			opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, GenerationFingerprint: "process-log-v1", Model: model, Tools: []tools.Definition{builtinDefinitionForSession(t, "execute")}, Operations: tools.Operations{Process: process, Artifacts: artifacts, OutputRedactor: func(_ context.Context, text string) (string, error) {
				return strings.ReplaceAll(text, "fixture-private-value", "[redacted]"), nil
			}}, ResourceScheduler: tools.NewResourceScheduler()}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			artifacts.before = func() {
				view := s.rt.manager.View()
				if len(view.Calls) != 1 {
					t.Error("log save preceded call acceptance")
					return
				}
				for _, call := range view.Calls {
					if call.Observation == nil || !call.Claimed || call.Observation.Status != "succeeded" || call.Observation.SideEffect != "confirmed" || !call.Observation.Terminated {
						t.Error("log save preceded durable original execution facts")
					}
				}
			}
			receipt := submitOutput(t, s)
			waitResumeCondition(t, func() bool { return model.Calls() == 2 || terminal(s.rt.manager.View().Traces[receipt.TraceID].State) })
			for _, observed := range s.rt.manager.View().Calls {
				projection, err := s.rt.LookupToolProjection(t.Context(), observed.Scope, observed.Call.CallID)
				if err != nil || projection == nil || projection.Observation != *observed.Observation {
					t.Fatal("active execution could not read committed projection", err)
				}
				cached, err := tools.NewExecutor(observed.Scope.Generation, opts.Tools, s.rt, sessionAuthorizer{rt: s.rt, scope: observed.Scope}, agent.NewBudget(config.DefaultLimits()))
				if err != nil {
					t.Fatal(err)
				}
				result, err := cached.Run(t.Context(), observed.Scope, observed.Call.ProviderCallID, observed.Call.Name, observed.Call.Arguments)
				if err != nil || result.Artifact != projection.Artifact || result.Content != projection.Content || result.LogError != projection.LogError || process.Starts() != 1 || artifacts.saves.Load() != 1 {
					t.Fatal("accepted call replay lost projection or repeated execution", err)
				}
			}
			close(gate)
			waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[receipt.TraceID].State) })
			view := s.rt.manager.View()
			if view.Traces[receipt.TraceID].State != "completed" || len(view.Calls) != 1 || len(view.ToolProjections) != 1 || process.Starts() != 1 || artifacts.saves.Load() != 1 || artifacts.Calls("save") != 0 {
				t.Fatalf("trace=%s calls=%d projections=%d starts=%d saves=%d legacy=%d", view.Traces[receipt.TraceID].State, len(view.Calls), len(view.ToolProjections), process.Starts(), artifacts.saves.Load(), artifacts.Calls("save"))
			}
			var call agent.ToolRecord
			for _, c := range view.Calls {
				call = c
			}
			p := view.ToolProjections[call.Call.CallID]
			if p.Observation != *call.Observation || p.Observation.ExitCode != 0 || p.Observation.SideEffect != "confirmed" || !p.Truncated || !strings.HasPrefix(p.Content, "HEAD") || !strings.HasSuffix(p.Content, "TAIL") {
				t.Fatal("projection changed execution facts or preview")
			}
			if fail {
				if p.Artifact.ID != "" || p.LogError == "" {
					t.Fatal("failed save published reference or hid error")
				}
			} else {
				if p.Artifact.ID == "" || p.LogError != "" {
					t.Fatal("successful save lost artifact")
				}
			}
			resultCount := 0
			for _, msg := range view.Messages {
				if msg.Kind != agent.KindToolResult {
					continue
				}
				resultCount++
				body, _ := json.Marshal(msg)
				if !strings.Contains(string(body), "TAIL") || strings.Contains(string(body), "fixture-private-value") || (!fail && !strings.Contains(string(body), p.Artifact.ID)) || (fail && !strings.Contains(string(body), "logError")) {
					t.Fatal("model result did not use safe committed projection")
				}
			}
			if resultCount != 1 || view.Traces[receipt.TraceID].Usage.ToolExecutions != 1 {
				t.Fatalf("results=%d tools=%d", resultCount, view.Traces[receipt.TraceID].Usage.ToolExecutions)
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			opts.Model = versionedPauseModel{testkit.NewFake(testkit.Step{Text: "must not run on open"})}
			reopened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close(context.Background()) })
			if _, ok := reopened.rt.opts.Store.(*jsonl.Store); !ok || reopened.rt.opts.Store == s.rt.opts.Store {
				t.Fatal("reopen did not use fresh disk store")
			}
			after := reopened.rt.manager.View()
			if after.ToolProjections[call.Call.CallID] != p || *after.Calls[call.Call.CallID].Observation != *call.Observation || process.Starts() != 1 || artifacts.saves.Load() != 1 {
				t.Fatal("reopen lost facts or repeated process/log save")
			}
			if _, err := reopened.Resume(t.Context(), ResumeCommand{TraceID: receipt.TraceID, ExpectedRevision: after.LastSeq}); err == nil {
				t.Fatal("terminal execution unexpectedly resumed")
			}
			if process.Starts() != 1 || artifacts.saves.Load() != 1 {
				t.Fatal("resume repeated terminal process or log save")
			}
			if !fail {
				reader, err := artifacts.OpenOutput(t.Context(), agent.OutputArtifactRead{Binding: artifacts.input.Binding, Ref: p.Artifact})
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(reader)
				_ = reader.Close()
				if err != nil || string(body) != strings.ReplaceAll(full, "fixture-private-value", "[redacted]") {
					t.Fatal("reopened artifact lost redacted full log", err)
				}
			}
		})
	}
}
