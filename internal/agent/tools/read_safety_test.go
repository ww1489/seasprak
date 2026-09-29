package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

type cancellingReadStore struct {
	*fixture.Memory
	cancel context.CancelFunc
	closed bool
	reads  int
}

func (p *cancellingReadStore) Open(ctx context.Context, r agent.ArtifactRead) (io.ReadCloser, error) {
	reader, err := p.Memory.Open(ctx, r)
	if err != nil {
		return nil, err
	}
	return &cancellingReadStream{ReadCloser: reader, owner: p}, nil
}

type cancellingReadStream struct {
	io.ReadCloser
	owner *cancellingReadStore
}

func (r *cancellingReadStream) Read(b []byte) (int, error) {
	r.owner.reads++
	n, err := r.ReadCloser.Read(b)
	r.owner.cancel()
	return n, err
}
func (r *cancellingReadStream) Close() error {
	r.owner.closed = true
	return r.ReadCloser.Close()
}

func TestReadCancellationStopsSnapshotAndClosesReader(t *testing.T) {
	for _, args := range []string{`{"path":"file"}`, `{"path":"file","byteLimit":100}`, `{"path":"file","offset":1}`} {
		m := fixture.NewMemory()
		m.SeedFile("file", []byte(strings.Repeat("中🙂", 100000)))
		ctx, cancel := context.WithCancel(t.Context())
		p := &cancellingReadStore{Memory: m, cancel: cancel}
		sink := &recordSink{found: true, rec: builtinAccepted(args, "read_file", "cancel-read")}
		e, err := NewExecutor("gen", []Definition{builtinByName(t, NewBuiltinDefinitions(BuiltinOptions{}), "read_file")}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Files: m, Artifacts: p}), WithResourceScheduler(NewResourceScheduler()))
		if err != nil {
			t.Fatal(err)
		}
		out, err := e.Run(ctx, agent.ExecutionScope{SessionID: "session"}, "cancel-read", "read_file", args)
		cancel()
		if err != nil || out.Status != "cancelled" || out.Executed || !p.closed || p.reads != 1 || m.Calls("read") != 1 || m.Calls("open") != 1 {
			t.Fatalf("cancellation lost: status=%s reads=%d closed=%v err=%v", out.Status, p.reads, p.closed, err)
		}
	}
}

func TestReadDeniedPolicyNeverCallsBackend(t *testing.T) {
	m := fixture.NewMemory()
	m.SeedFile("file", []byte("secret fixture content"))
	const args = `{"path":"file","byteLimit":4}`
	sink := &recordSink{found: true, rec: builtinAccepted(args, "read_file", "denied-read")}
	e, err := NewExecutor("gen", []Definition{builtinByName(t, NewBuiltinDefinitions(BuiltinOptions{}), "read_file")}, sink, deny{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Files: m, Artifacts: m}), WithResourceScheduler(NewResourceScheduler()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "denied-read", "read_file", args)
	if err != nil || out.Status != "denied" || out.Executed || m.Calls("read") != 0 || m.Calls("open") != 0 {
		t.Fatalf("read bypassed policy: %+v err=%v", out, err)
	}
}

func TestReadFailureSanitizesBackendDiagnostics(t *testing.T) {
	const marker = "PRIVATE_DIAGNOSTIC_MARKER"
	cases := []struct {
		name      string
		readErr   error
		openErr   error
		code      string
		status    string
		content   string
		readCalls int
		openCalls int
	}{
		{
			name:      "read product error",
			readErr:   product.NewError(product.CodeStateConflict, marker),
			code:      product.CodeStateConflict,
			status:    "failed",
			content:   "controlled file read failed",
			readCalls: 1,
		},
		{
			name:      "open joined cancellation and product error",
			openErr:   errors.Join(context.Canceled, product.NewError(product.CodeNotFound, marker)),
			code:      product.CodeNotFound,
			status:    "cancelled",
			content:   "controlled file read cancelled",
			readCalls: 1,
			openCalls: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := &readFailureBackend{Memory: fixture.NewMemory(), readErr: tc.readErr, openErr: tc.openErr}
			backend.SeedFile("file", []byte("body"))
			args := `{"path":"file"}`
			sink := &recordSink{found: true, rec: builtinAccepted(args, "read_file", "read-failure-safety")}
			e, err := NewExecutor("gen", []Definition{builtinByName(t, NewBuiltinDefinitions(BuiltinOptions{}), "read_file")}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Files: backend, Artifacts: backend}), WithResourceScheduler(NewResourceScheduler()))
			if err != nil {
				t.Fatal(err)
			}
			out, err := e.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "read-failure-safety", "read_file", args)
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != tc.status || out.ExecutionError != tc.code || out.Content != tc.content {
				t.Fatalf("unsafe read failure: status=%q code=%q content=%q", out.Status, out.ExecutionError, out.Content)
			}
			if strings.Contains(out.Content, marker) || strings.Contains(out.ModelContent(), marker) {
				t.Fatalf("backend diagnostic reached model output: %q", out.ModelContent())
			}
			var observation *agent.ToolObservation
			for _, fact := range sink.facts {
				if fact.Kind != "tool_observation" {
					continue
				}
				if strings.Contains(string(fact.Payload), marker) {
					t.Fatalf("backend diagnostic persisted in tool_observation: %s", fact.Payload)
				}
				var record agent.ToolRecord
				if err := json.Unmarshal(fact.Payload, &record); err != nil {
					t.Fatal(err)
				}
				observation = record.Observation
			}
			if observation == nil || observation.Status != tc.status || observation.ExecutionError != tc.code || observation.Content != tc.content {
				t.Fatalf("unsafe persisted observation: %+v", observation)
			}
			if backend.readCalls != tc.readCalls || backend.openCalls != tc.openCalls {
				t.Fatalf("unexpected backend calls: read=%d open=%d", backend.readCalls, backend.openCalls)
			}
		})
	}
}

type readFailureBackend struct {
	*fixture.Memory
	readErr     error
	openErr     error
	streamErr   error
	readCalls   int
	openCalls   int
	streamCalls int
	closed      bool
}

func (b *readFailureBackend) Read(ctx context.Context, r agent.ReadRequest) (agent.ReadResult, error) {
	b.readCalls++
	if b.readErr != nil {
		return agent.ReadResult{}, b.readErr
	}
	return b.Memory.Read(ctx, r)
}

func (b *readFailureBackend) Open(ctx context.Context, r agent.ArtifactRead) (io.ReadCloser, error) {
	b.openCalls++
	if b.openErr != nil {
		return nil, b.openErr
	}
	reader, err := b.Memory.Open(ctx, r)
	if err != nil || b.streamErr == nil {
		return reader, err
	}
	return &failingReadStream{ReadCloser: reader, backend: b}, nil
}

type failingReadStream struct {
	io.ReadCloser
	backend *readFailureBackend
}

func (r *failingReadStream) Read([]byte) (int, error) {
	r.backend.streamCalls++
	return 0, r.backend.streamErr
}

func (r *failingReadStream) Close() error {
	r.backend.closed = true
	return r.ReadCloser.Close()
}

func TestReadBackendErrorsStayIsolatedAcrossStages(t *testing.T) {
	const marker = "PRIVATE_DIAGNOSTIC_MARKER"
	cases := []struct {
		name, code, status string
		err                error
	}{
		{"product", product.CodeStateConflict, "failed", product.NewError(product.CodeStateConflict, marker)},
		{"cancel", product.CodeNotFound, "cancelled", errors.Join(context.Canceled, product.NewError(product.CodeNotFound, marker))},
		{"deadline", product.CodeResourceUnavailable, "cancelled", errors.Join(context.DeadlineExceeded, errors.New(marker))},
	}
	for _, reason := range []string{"utf8_boundary", "invalid_utf8", "byte_limit_too_small"} {
		cases = append(cases, struct {
			name, code, status string
			err                error
		}{reason, product.CodeInvalidArgument, "failed", &product.Error{Code: product.CodeInvalidArgument, Message: reason, Details: marker, Refs: map[string]string{"diagnostic": marker}}})
	}
	for _, stage := range []string{"read", "open", "stream"} {
		for _, tc := range cases {
			t.Run(stage+"/"+tc.name, func(t *testing.T) {
				b := &readFailureBackend{Memory: fixture.NewMemory()}
				b.SeedFile("file", []byte("body"))
				switch stage {
				case "read":
					b.readErr = tc.err
				case "open":
					b.openErr = tc.err
				case "stream":
					b.streamErr = tc.err
				}
				out, sink := runReadRange(t, b, b, `{"path":"file"}`)
				content := "controlled file read failed"
				if tc.status == "cancelled" {
					content = "controlled file read cancelled"
				}
				if out.Status != tc.status || out.ExecutionError != tc.code || out.Content != content || out.Executed || out.SideEffect != "none" {
					t.Fatal("read failure changed status/code or exposed backend diagnostics")
				}
				// Failed pipeline tools encode the whole Outcome, not only Content.
				model, err := json.Marshal(out)
				if err != nil || strings.Contains(string(model), marker) || strings.Contains(out.ModelContent(), marker) {
					t.Fatal("backend diagnostics reached model text")
				}
				observations := 0
				for _, fact := range sink.facts {
					if fact.Kind != "tool_observation" {
						continue
					}
					observations++
					var record agent.ToolRecord
					if strings.Contains(string(fact.Payload), marker) || json.Unmarshal(fact.Payload, &record) != nil || record.Observation == nil {
						t.Fatal("unsafe or invalid durable observation")
					}
					obs := record.Observation
					if !record.Claimed || obs.Status != tc.status || obs.ExecutionError != tc.code || obs.Content != content || obs.Executed || obs.SideEffect != "none" || strings.Contains(obs.ModelContent(), marker) {
						t.Fatal("durable observation lost failure facts or exposed diagnostics")
					}
				}
				wantOpen, wantStream := 1, 0
				if stage == "read" {
					wantOpen = 0
				}
				if stage == "stream" {
					wantStream = 1
				}
				if observations != 1 || b.readCalls != 1 || b.openCalls != wantOpen || b.streamCalls != wantStream || b.closed != (stage == "stream") {
					t.Fatalf("unexpected lifecycle: observations=%d read=%d open=%d stream=%d closed=%v", observations, b.readCalls, b.openCalls, b.streamCalls, b.closed)
				}
			})
		}
	}
}
