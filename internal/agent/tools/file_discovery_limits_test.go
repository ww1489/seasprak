package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
	"strings"
	"testing"
	"unicode/utf8"
)

func discoveryResult(t *testing.T, out Outcome) discoveryPage {
	t.Helper()
	var p discoveryPage
	if out.Status != "succeeded" || json.Unmarshal([]byte(out.Content), &p) != nil || !utf8.ValidString(out.Content) || len(out.Content) > discoveryBytes {
		t.Fatalf("invalid bounded result status=%s bytes=%d", out.Status, len(out.Content))
	}
	return p
}
func TestFileDiscoveryLimitsOrderingAndModes(t *testing.T) {
	m := fixture.NewMemory()
	for i := 1001; i >= 0; i-- {
		m.SeedFile(fmt.Sprintf("root/%04d.go", i), []byte("针"+strings.Repeat("界", 600)+"\nneedle\nneedle"))
	}
	p := discoveryResult(t, runDiscovery(t, m, "ls", `{"root":"root"}`))
	if p.Returned <= 0 || p.Returned > 500 || !p.Truncated || p.Entries[0].Name != "0000.go" {
		t.Fatalf("list returned=%d truncated=%v", p.Returned, p.Truncated)
	}
	p = discoveryResult(t, runDiscovery(t, m, "glob", `{"root":"root","pattern":"**/*.go"}`))
	if p.Returned <= 0 || p.Returned > 1000 || !p.Truncated || p.Matches[0].Identity != "root/0000.go" {
		t.Fatalf("glob=%+v", p)
	}
	p = discoveryResult(t, runDiscovery(t, m, "grep", `{"root":"root","query":"针"}`))
	if !p.Truncated || p.Returned >= 100 || len([]rune(p.Matches[0].Preview)) != 500 {
		t.Fatalf("grep returned=%d truncated=%v", p.Returned, p.Truncated)
	}
	for _, mode := range []string{"content", "files_with_matches", "count"} {
		p = discoveryResult(t, runDiscovery(t, m, "grep", fmt.Sprintf(`{"root":"root","query":"needle","output_mode":%q,"offset":1,"head_limit":1}`, mode)))
		if p.Returned != 1 || !p.Truncated {
			t.Fatalf("mode=%s result=%+v", mode, p)
		}
		switch mode {
		case "content":
			if p.Matches[0].Line != 3 {
				t.Fatal(p)
			}
		case "files_with_matches":
			if p.Files[0] != "root/0001.go" {
				t.Fatal(p)
			}
		case "count":
			if p.Counts[0].Count != 2 || p.Counts[0].Identity != "root/0001.go" {
				t.Fatal(p)
			}
		}
	}
}
func TestFileDiscoveryEmptyErrorsAndGlobFilter(t *testing.T) {
	m := fixture.NewMemory()
	m.SeedFile("r/a.go", []byte("NEEDLE"))
	m.SeedFile("r/b.txt", []byte("needle"))
	m.SeedFile("r-other/no.go", []byte("needle"))
	p := discoveryResult(t, runDiscovery(t, m, "grep", `{"root":"r","query":"needle","glob":"*.go","case_insensitive":true}`))
	if p.Returned != 1 || p.Matches[0].Identity != "r/a.go" {
		t.Fatal(p)
	}
	p = discoveryResult(t, runDiscovery(t, m, "glob", `{"root":"empty","pattern":"**/*"}`))
	if p.Returned != 0 || p.Truncated {
		t.Fatal(p)
	}
	for _, name := range []string{"grep", "glob"} {
		key := "query"
		if name == "glob" {
			key = "pattern"
		}
		before := m.Calls("search")
		out := runDiscovery(t, m, name, fmt.Sprintf(`{"root":"empty",%q:"["}`, key))
		if out.ExecutionError != "invalid_argument" || out.SideEffect != "none" || m.Calls("search") != before+1 {
			t.Fatalf("out=%+v", out)
		}
	}
}

type discoveryDeny struct{}

func (discoveryDeny) Authorize(context.Context, agent.FrozenCall) (agent.Decision, error) {
	return agent.DecisionDeny, nil
}
func TestFileDiscoveryDeniedAndCancelledZeroCalls(t *testing.T) {
	for _, name := range []string{"ls", "glob", "grep", "edit_file"} {
		t.Run(name, func(t *testing.T) {
			m := fixture.NewMemory()
			args := map[string]string{"ls": `{"root":"r"}`, "glob": `{"root":"r","pattern":"*"}`, "grep": `{"root":"r","query":"x"}`, "edit_file": `{"path":"a","old_string":"x","new_string":"y"}`}[name]
			def := builtinByName(t, NewBuiltinDefinitions(BuiltinOptions{}), name)
			e, err := NewExecutor("g", []Definition{def}, &recordSink{found: true, rec: builtinAccepted(args, name, "provider")}, discoveryDeny{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Files: m}), WithResourceScheduler(NewResourceScheduler()))
			if err != nil {
				t.Fatal(err)
			}
			out, err := e.Run(t.Context(), agent.ExecutionScope{}, "provider", name, args)
			if err != nil || out.Status != "denied" {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = e.Run(ctx, agent.ExecutionScope{}, "provider", name, args)
			if err != context.Canceled {
				t.Fatal(err)
			}
			if m.Calls("list")+m.Calls("search")+m.Calls("edit") != 0 {
				t.Fatal("unauthorized backend call")
			}
		})
	}
}
func TestFileExactEditVersionAndDeletion(t *testing.T) {
	m := fixture.NewMemory()
	v := m.SeedFile("a", []byte("你好\r\n"))
	out := runDiscovery(t, m, "edit_file", `{"path":"a","old_string":"你好","new_string":"","expectedVersion":"stale"}`)
	b, current, _ := m.Snapshot("a")
	if out.ExecutionError != "state_conflict" || out.SideEffect != "none" || current != v || string(b) != "你好\r\n" {
		t.Fatalf("out=%+v", out)
	}
	out = runDiscovery(t, m, "edit_file", fmt.Sprintf(`{"path":"a","old_string":"你好","new_string":"","expectedVersion":%q}`, v))
	b, _, _ = m.Snapshot("a")
	if out.Status != "succeeded" || string(b) != "\r\n" || m.Calls("edit") != 2 {
		t.Fatalf("out=%+v", out)
	}
}
