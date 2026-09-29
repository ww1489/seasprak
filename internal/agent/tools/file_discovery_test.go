package tools

import (
	"encoding/json"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
	"testing"
)

func runDiscovery(t *testing.T, m *fixture.Memory, name, args string) Outcome {
	t.Helper()
	def := builtinByName(t, NewBuiltinDefinitions(BuiltinOptions{}), name)
	e, err := NewExecutor("gen", []Definition{def}, &recordSink{found: true, rec: builtinAccepted(args, name, "provider")}, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Files: m, Artifacts: m}), WithResourceScheduler(NewResourceScheduler()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "provider", name, args)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestFileDiscoveryActualBackend(t *testing.T) {
	for _, tc := range []struct{ name, args string }{
		{"ls", `{"root":"root"}`},
		{"glob", `{"root":"root","pattern":"**/*.go"}`},
		{"grep", `{"root":"root","query":"needle","output_mode":"content","head_limit":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixture.NewMemory()
			m.SeedFile("root/z.go", []byte("needle"))
			m.SeedFile("root/a.go", []byte("needle"))
			out := runDiscovery(t, m, tc.name, tc.args)
			if out.Status != "succeeded" || !json.Valid([]byte(out.Content)) || !out.Executed {
				t.Fatalf("out=%+v", out)
			}
			method := "search"
			if tc.name == "ls" {
				method = "list"
			}
			if m.Calls(method) != 1 {
				t.Fatalf("calls=%d", m.Calls(method))
			}
		})
	}
}
func TestFileExactEditPublicArguments(t *testing.T) {
	m := fixture.NewMemory()
	m.SeedFile("a", []byte("old old"))
	out := runDiscovery(t, m, "edit_file", `{"path":"a","old_string":"old","new_string":"new"}`)
	if out.ExecutionError != "state_conflict" || out.SideEffect != "none" || m.Calls("edit") != 1 {
		t.Fatalf("out=%+v calls=%d", out, m.Calls("edit"))
	}
	out = runDiscovery(t, m, "edit_file", `{"path":"a","old_string":"old","new_string":"new","replace_all":true}`)
	b, _, _ := m.Snapshot("a")
	if out.Status != "succeeded" || string(b) != "new new" || m.Calls("edit") != 2 {
		t.Fatalf("out=%+v content=%q", out, b)
	}
}
