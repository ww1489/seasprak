package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

type testkitProcessProbe struct {
	*fixture.Process
	request agent.AuthorizedProcess
	mutate  func(*agent.AuthorizedProcess)
	err     error
}

func (p *testkitProcessProbe) Execute(ctx context.Context, r agent.AuthorizedProcess, s agent.ProgressSink) (agent.ProcessObservation, error) {
	p.request = r
	if p.mutate != nil {
		p.mutate(&r)
	}
	obs, err := p.Process.Execute(ctx, r, s)
	p.err = err
	return obs, err
}

func TestTestkitOperationsProcessLifecycle(t *testing.T) {
	for _, finish := range []string{"release", "cancel", "stop"} {
		t.Run(finish, func(t *testing.T) {
			release := make(chan struct{})
			p := &testkitProcessProbe{Process: fixture.NewProcess(release, "inline output")}
			def := builtinByName(t, NewBuiltinDefinitions(BuiltinOptions{}), "execute")
			args := `{"argv":["fixture-command"],"cwd":"workspace"}`
			sink := &recordSink{found: true, rec: builtinAccepted(args, "execute", "provider")}
			e, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Process: p}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan Outcome, 1)
			go func() {
				out, err := e.Run(ctx, agent.ExecutionScope{SessionID: "session"}, "provider", "execute", args)
				if err != nil {
					t.Errorf("run: %v", err)
				}
				done <- out
			}()
			var ref agent.ExecutionRef
			select {
			case ref = <-p.Started():
			case <-ctx.Done():
				t.Fatal("process did not enter")
			}
			select {
			case <-done:
				t.Fatal("process completed before control signal")
			default:
			}
			switch finish {
			case "release":
				close(release)
			case "cancel":
				cancel()
			case "stop":
				obs, err := p.Stop(t.Context(), ref)
				if err != nil || !obs.Terminated || obs.SideEffect != "none" {
					t.Fatalf("stop=%+v err=%v", obs, err)
				}
			}
			var out Outcome
			select {
			case out = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("process failed to settle")
			}
			if !out.Executed || out.SideEffect != "none" || p.Starts() != 1 || !hasIntent(sink) {
				t.Fatalf("out=%+v starts=%d", out, p.Starts())
			}
			if finish == "release" && (out.Status != "succeeded" || out.Content != "inline output") {
				t.Fatalf("out=%+v", out)
			}
			if finish != "release" && out.Status != "failed" {
				t.Fatalf("out=%+v", out)
			}
			_, err = p.Process.Execute(t.Context(), p.request, nil)
			pe, ok := product.AsError(err)
			if !ok || pe.Code != product.CodePermissionDenied || p.Starts() != 1 {
				t.Fatalf("copy err=%v starts=%d", err, p.Starts())
			}
			_, err = p.Stop(t.Context(), agent.ExecutionRef{ID: "missing"})
			pe, ok = product.AsError(err)
			if !ok || pe.Code != product.CodeNotFound {
				t.Fatalf("unknown stop=%v", err)
			}
		})
	}
}

func TestTestkitOperationsProcessRejectsChangedRequest(t *testing.T) {
	for _, field := range []string{"argv", "cwd", "shell", "env", "stdin", "mounts", "temp", "limit", "frozen"} {
		t.Run(field, func(t *testing.T) {
			release := make(chan struct{})
			close(release)
			p := &testkitProcessProbe{Process: fixture.NewProcess(release, "ok")}
			p.mutate = func(r *agent.AuthorizedProcess) {
				switch field {
				case "argv":
					r.Argv = []string{"other"}
				case "cwd":
					r.Cwd = "other"
				case "shell":
					r.Shell = "other"
				case "env":
					r.EnvironmentRef = "other"
				case "stdin":
					r.StdinRef = "other"
				case "mounts":
					r.Mounts = []agent.ExecutionMount{{Target: "other"}}
				case "temp":
					r.TempRootRef = "other"
				case "limit":
					r.OutputLimitBytes++
				case "frozen":
					r.Authorization.Frozen.CallID = "other"
				}
			}
			def := builtinByName(t, NewBuiltinDefinitions(BuiltinOptions{}), "execute")
			args := `{"argv":["fixture-command"]}`
			e, err := NewExecutor("gen", []Definition{def}, &recordSink{found: true, rec: builtinAccepted(args, "execute", "provider")}, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Process: p}), WithResourceScheduler(NewResourceScheduler()))
			if err != nil {
				t.Fatal(err)
			}
			out, err := e.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "provider", "execute", args)
			pe, ok := product.AsError(p.err)
			if err != nil || !ok || pe.Code != product.CodePermissionDenied || out.Executed || out.SideEffect != "none" || p.Starts() != 0 {
				t.Fatalf("out=%+v err=%v backend=%v starts=%d", out, err, p.err, p.Starts())
			}
		})
	}
}

func TestTestkitOperationsRealBuiltinWriteRead(t *testing.T) {
	m := fixture.NewMemory()
	ref := m.RegisterContent([]byte("complete content"))
	for _, step := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"write_file", map[string]any{"path": "file", "contentRef": ref}, "file"},
		{"read_file", map[string]any{"path": "file"}, "complete content"},
	} {
		raw, _ := json.Marshal(step.args)
		def := builtinByName(t, NewBuiltinDefinitions(BuiltinOptions{}), step.name)
		sink := &recordSink{found: true, rec: builtinAccepted(string(raw), step.name, "provider")}
		e, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Files: m, Artifacts: m}), WithResourceScheduler(NewResourceScheduler()))
		if err != nil {
			t.Fatal(err)
		}
		out, err := e.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "provider", step.name, string(raw))
		if err != nil || out.Status != "succeeded" || out.Content != step.want || !hasIntent(sink) {
			t.Fatalf("out=%+v err=%v", out, err)
		}
	}
	if m.Calls("write") != 1 || m.Calls("read") != 1 || m.Calls("open") != 1 {
		t.Fatal("builtins did not use shared operations")
	}
}
