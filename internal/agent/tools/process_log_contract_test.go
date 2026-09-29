package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

type processLogProcess struct {
	observation agent.ProcessObservation
	request     agent.AuthorizedProcess
	calls       int
}

func (p *processLogProcess) Execute(ctx context.Context, req agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := req.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	p.request = req
	p.calls++
	return p.observation, nil
}
func (*processLogProcess) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return memoryBackendCapabilities("process-log-fake"), nil
}
func (*processLogProcess) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}

type processLogStore struct {
	calls, legacyCalls int
	input              agent.OutputArtifactInput
	save               func(context.Context, agent.OutputArtifactInput) (agent.ArtifactRef, error)
}

func (s *processLogStore) Save(ctx context.Context, in agent.ArtifactInput) (agent.ArtifactRef, error) {
	s.legacyCalls++
	return agent.ArtifactRef{}, in.Authorization.Validate(ctx)
}
func (*processLogStore) Open(context.Context, agent.ArtifactRead) (io.ReadCloser, error) {
	return nil, product.NewError(product.CodeNotFound, "not used")
}
func (s *processLogStore) SaveOutput(ctx context.Context, in agent.OutputArtifactInput) (agent.ArtifactRef, error) {
	s.calls++
	s.input = in
	if s.save != nil {
		return s.save(ctx, in)
	}
	return processLogRef(in), nil
}
func (*processLogStore) OpenOutput(context.Context, agent.OutputArtifactRead) (io.ReadCloser, error) {
	return nil, product.NewError(product.CodeNotFound, "not used")
}
func processLogRef(in agent.OutputArtifactInput) agent.ArtifactRef {
	hash := sha256.Sum256([]byte(in.Content))
	return agent.ArtifactRef{ID: "artifact:full", SessionID: in.Binding.SessionID, Environment: in.Binding.Environment, Hash: hex.EncodeToString(hash[:]), Size: int64(len(in.Content)), Available: true}
}
func processLogExecutor(t *testing.T, p *processLogProcess, s *processLogStore, sink *recordSink) *Executor {
	t.Helper()
	def := Definition{Name: "execute", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: ExecutionDescription{BackendID: "process-operations", Effect: "unknown", Argv: []string{"runner"}}}
	e, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Process: p, Artifacts: s, OutputRedactor: func(_ context.Context, text string) (string, error) {
		return strings.ReplaceAll(text, "fixture-private-value", "[redacted]"), nil
	}}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func processLogSink() *recordSink {
	rec := builtinAccepted(`{}`, "execute", "provider-log")
	rec.Scope.SessionID = "session"
	return &recordSink{found: true, rec: rec}
}
func processLogObservation(content string) agent.ProcessObservation {
	return agent.ProcessObservation{Started: true, Terminated: true, Content: content, ContentRef: "private:log", SideEffect: "confirmed"}
}
func runProcessLog(t *testing.T, e *Executor) (Outcome, error) {
	t.Helper()
	return e.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "provider-log", "execute", `{}`)
}
func TestProcessLogShortOutputDoesNotSave(t *testing.T) {
	p := &processLogProcess{observation: processLogObservation("short output\n")}
	s := &processLogStore{}
	sink := processLogSink()
	out, err := runProcessLog(t, processLogExecutor(t, p, s, sink))
	if err != nil || out.Status != "succeeded" || out.Content != "short output\n" || out.SideEffect != "confirmed" || !out.Executed || p.calls != 1 || s.calls != 0 || s.legacyCalls != 0 {
		t.Fatalf("status=%s effect=%s process=%d saves=%d legacy=%d err=%v", out.Status, out.SideEffect, p.calls, s.calls, s.legacyCalls, err)
	}
}
func TestProcessLogSaveFailurePreservesExecution(t *testing.T) {
	for _, exit := range []int{0, 23} {
		t.Run(string(rune('a'+exit)), func(t *testing.T) {
			p := &processLogProcess{observation: processLogObservation(strings.Repeat("log line\n", 2100))}
			p.observation.ExitCode = exit
			s := &processLogStore{save: func(context.Context, agent.OutputArtifactInput) (agent.ArtifactRef, error) {
				return agent.ArtifactRef{}, errors.New("private backend error must not escape")
			}}
			sink := processLogSink()
			out, err := runProcessLog(t, processLogExecutor(t, p, s, sink))
			want := "succeeded"
			if exit != 0 {
				want = "failed"
			}
			if err != nil || out.Status != want || out.SideEffect != "confirmed" || !out.Executed || out.ExitCode != exit || !out.Terminated || !strings.HasPrefix(out.LogError, product.CodeResourceUnavailable+":") || out.Artifact.ID != "" || p.calls != 1 || s.calls != 1 || s.legacyCalls != 0 {
				t.Fatalf("status=%s exit=%d effect=%s saved=%d legacy=%d logError=%q err=%v", out.Status, out.ExitCode, out.SideEffect, s.calls, s.legacyCalls, out.LogError, err)
			}
			obs := lastObservation(t, sink)
			if obs.Status != want || obs.ExitCode != exit || obs.SideEffect != "confirmed" || !obs.Executed || !obs.Terminated {
				t.Fatal("raw process facts changed")
			}
		})
	}
}
func TestProcessLogObservationPrecedesArtifactSave(t *testing.T) {
	p := &processLogProcess{observation: processLogObservation("HEAD\nfixture-private-value\n" + strings.Repeat("中文🚀\n", 2100) + "TAIL")}
	sink := processLogSink()
	before := false
	s := &processLogStore{save: func(_ context.Context, in agent.OutputArtifactInput) (agent.ArtifactRef, error) {
		before = hasObservation(t, sink, "succeeded")
		return processLogRef(in), nil
	}}
	out, err := runProcessLog(t, processLogExecutor(t, p, s, sink))
	if err != nil || !before || p.calls != 1 || s.calls != 1 || out.Artifact != processLogRef(s.input) || out.LogError != "" || !out.Truncated || !utf8.ValidString(out.Content) || !strings.HasPrefix(out.Content, "HEAD") || !strings.HasSuffix(out.Content, "TAIL") {
		t.Fatalf("before=%v saves=%d ref=%+v error=%v", before, s.calls, out.Artifact, err)
	}
	if strings.Contains(s.input.Content, "fixture-private-value") || !strings.Contains(s.input.Content, "[redacted]") || len(s.input.Content) <= len(out.Content) {
		t.Fatal("saved log was not redacted full text")
	}
	for _, f := range sink.facts {
		if strings.Contains(string(f.Payload), "fixture-private-value") {
			t.Fatal("original observation or projection leaked unredacted text")
		}
	}
	var projected agent.ToolOutputProjection
	for _, f := range sink.facts {
		if f.Kind == "tool_output_projection" {
			if err := json.Unmarshal(f.Payload, &projected); err != nil {
				t.Fatal(err)
			}
		}
	}
	if projected.Artifact != out.Artifact || projected.CallID != sink.rec.Call.CallID || projected.Observation != lastObservation(t, sink) {
		t.Fatal("projection did not bind original observation and complete artifact")
	}
	var model map[string]any
	if json.Unmarshal([]byte(out.ModelContent()), &model) != nil || model["artifact"] == nil {
		t.Fatal("model projection omitted complete artifact")
	}
}
func TestProcessLogObservationFailurePreventsArtifactSave(t *testing.T) {
	p := &processLogProcess{observation: processLogObservation(strings.Repeat("log line\n", 2100))}
	s := &processLogStore{}
	sink := processLogSink()
	commitErr := errors.New("observation commit failed")
	sink.beforeCommit = func(f agent.Fact) error {
		if f.Kind == "tool_observation" {
			return commitErr
		}
		return nil
	}
	out, err := runProcessLog(t, processLogExecutor(t, p, s, sink))
	if !errors.Is(err, commitErr) || p.calls != 1 || s.calls != 0 || out.Artifact.ID != "" {
		t.Fatalf("process=%d saves=%d published=%v err=%v", p.calls, s.calls, out.Artifact.ID != "", err)
	}
}
func TestProcessLogMustNotReuseConsumedExecutionTicket(t *testing.T) {
	p := &processLogProcess{observation: processLogObservation(strings.Repeat("log line\n", 2100))}
	s := &processLogStore{}
	sink := processLogSink()
	out, err := runProcessLog(t, processLogExecutor(t, p, s, sink))
	pe, ok := product.AsError(p.request.Authorization.Validate(t.Context()))
	if err != nil || out.Status != "succeeded" || out.SideEffect != "confirmed" || p.calls != 1 || s.calls != 1 || s.legacyCalls != 0 || !ok || pe.Code != product.CodePermissionDenied {
		t.Fatalf("status=%s process=%d saves=%d legacy=%d err=%v", out.Status, p.calls, s.calls, s.legacyCalls, err)
	}
	claims := 0
	for _, f := range sink.facts {
		if f.Kind == "tool_intent" {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("claims=%d", claims)
	}
}
func TestProcessLogRedactionUnavailableNeverSavesFullText(t *testing.T) {
	for _, kind := range []string{"missing", "error", "panic"} {
		t.Run(kind, func(t *testing.T) {
			p := &processLogProcess{observation: processLogObservation(strings.Repeat("fixture-private-value\n", 2100))}
			s := &processLogStore{}
			sink := processLogSink()
			e := processLogExecutor(t, p, s, sink)
			e.operations.OutputRedactor = nil
			if kind == "error" {
				e.operations.OutputRedactor = func(context.Context, string) (string, error) { return "", errors.New("private error") }
			}
			if kind == "panic" {
				e.operations.OutputRedactor = func(context.Context, string) (string, error) { panic("private panic") }
			}
			out, err := runProcessLog(t, e)
			if err != nil || out.Status != "succeeded" || out.SideEffect != "confirmed" || out.LogError == "" || s.calls != 0 || out.Artifact.ID != "" {
				t.Fatalf("status=%s saves=%d logError=%q err=%v", out.Status, s.calls, out.LogError, err)
			}
			for _, f := range sink.facts {
				if strings.Contains(string(f.Payload), "fixture-private-value") {
					t.Fatal("redaction failure persisted original text")
				}
			}
		})
	}
}
func TestProcessLogProjectionFailureDoesNotPublishRef(t *testing.T) {
	p := &processLogProcess{observation: processLogObservation(strings.Repeat("log line\n", 2100))}
	s := &processLogStore{}
	sink := processLogSink()
	sink.beforeCommit = func(f agent.Fact) error {
		if f.Kind == "tool_output_projection" {
			return errors.New("projection commit failed")
		}
		return nil
	}
	e := processLogExecutor(t, p, s, sink)
	sink.afterCommit = func(f agent.Fact) {
		if f.Kind == "tool_observation" {
			if err := json.Unmarshal(f.Payload, &sink.rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	out, err := runProcessLog(t, e)
	if err != nil || out.Status != "succeeded" || out.SideEffect != "confirmed" || out.Artifact.ID != "" || out.LogError == "" || s.calls != 1 || !hasObservation(t, sink, "succeeded") {
		t.Fatalf("status=%s saves=%d ref=%s logError=%q err=%v", out.Status, s.calls, out.Artifact.ID, out.LogError, err)
	}
	cached, err := runProcessLog(t, e)
	if err != nil || cached != out || p.calls != 1 || s.calls != 1 {
		t.Fatal("projection failure replayed execution or log save", err)
	}
}

func TestProcessLogBytePreviewValidatesArtifactReferences(t *testing.T) {
	for _, field := range []string{"valid", "session", "environment", "hash", "size", "availability", "identity", "panic"} {
		t.Run(field, func(t *testing.T) {
			p := &processLogProcess{observation: processLogObservation("HEAD" + strings.Repeat("中🙂", 10000) + "TAIL")}
			s := &processLogStore{save: func(_ context.Context, in agent.OutputArtifactInput) (agent.ArtifactRef, error) {
				ref := processLogRef(in)
				switch field {
				case "session":
					ref.SessionID = "other"
				case "environment":
					ref.Environment = "other"
				case "hash":
					ref.Hash = "changed"
				case "size":
					ref.Size++
				case "availability":
					ref.Available = false
				case "identity":
					ref.ID = ""
				case "panic":
					panic("private backend diagnostics")
				}
				return ref, nil
			}}
			out, err := runProcessLog(t, processLogExecutor(t, p, s, processLogSink()))
			if err != nil || out.Status != "succeeded" || out.SideEffect != "confirmed" || !out.Truncated || !utf8.ValidString(out.Content) || len(out.Content) > DefaultOutputLimits().MaxBytes || !strings.HasPrefix(out.Content, "HEAD") || !strings.HasSuffix(out.Content, "TAIL") || s.calls != 1 {
				t.Fatal("byte preview or execution facts changed", err)
			}
			if field == "valid" {
				if out.Artifact.ID == "" || out.LogError != "" {
					t.Fatal("valid artifact was rejected")
				}
			} else {
				code := product.CodeStateConflict
				if field == "panic" {
					code = product.CodeResourceUnavailable
				}
				if out.Artifact.ID != "" || !strings.HasPrefix(out.LogError, code+":") {
					t.Fatal("invalid artifact was published or error code changed")
				}
			}
		})
	}
}

func TestProcessLogReferenceOnlyOutputReportsUnavailable(t *testing.T) {
	p := &processLogProcess{observation: processLogObservation("")}
	s := &processLogStore{}
	out, err := runProcessLog(t, processLogExecutor(t, p, s, processLogSink()))
	if err != nil || out.Status != "succeeded" || out.SideEffect != "confirmed" || out.Artifact.ID != "" || out.LogError == "" || s.calls != 0 || s.legacyCalls != 0 {
		t.Fatal("private content reference was treated as a saved artifact", err)
	}
}
