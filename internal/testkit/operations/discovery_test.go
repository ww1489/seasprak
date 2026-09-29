package operations_test

import (
	"context"
	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
	"testing"
)

func TestMemoryLiteralEditFrozenBinding(t *testing.T) {
	for _, field := range []string{"old", "new", "all", "legacy"} {
		t.Run(field, func(t *testing.T) {
			m := fixture.NewMemory()
			version := m.SeedFile("a", []byte("old"))
			p := &filesProbe{Memory: m, edit: func(ctx context.Context, r agent.AuthorizedFileEdit) (agent.FileEffect, error) {
				switch field {
				case "old":
					r.OldString = "other"
				case "new":
					r.NewString = "other"
				case "all":
					r.ReplaceAll = true
				case "legacy":
					r.PatchRef = m.RegisterPatch(fixture.Patch{Old: "old", New: "other"})
				}
				effect, err := m.Edit(ctx, r)
				code(t, err, product.CodePermissionDenied)
				return effect, err
			}}
			out := run(t, m, "edit_file", map[string]any{"path": "a", "old_string": "old", "new_string": "new", "expectedVersion": version}, p, m)
			b, v, _ := m.Snapshot("a")
			if out.ExecutionError != product.CodePermissionDenied || out.Executed || out.SideEffect != "none" || string(b) != "old" || v != version || m.Calls("edit") != 1 {
				t.Fatalf("out=%+v", out)
			}
		})
	}
}
func TestMemoryDiscoveryLogicalPathsAndCancellation(t *testing.T) {
	m := fixture.NewMemory()
	m.SeedFile("root/a.go", []byte("needle"))
	m.SeedFile("root/sub/b.go", []byte("other"))
	m.SeedFile("root-other/c.go", []byte("needle"))
	listed, err := m.List(t.Context(), agent.ListRequest{Root: "root"})
	if err != nil || len(listed.Entries) != 2 || listed.Entries[0].Name != "a.go" || listed.Entries[1].Kind != "directory" {
		t.Fatalf("list=%+v err=%v", listed, err)
	}
	found, err := m.Search(t.Context(), agent.SearchRequest{Root: "root", Kind: "glob", Query: "**/*.go"})
	if err != nil || len(found.Matches) != 2 {
		t.Fatalf("glob=%+v err=%v", found, err)
	}
	found, err = m.Search(t.Context(), agent.SearchRequest{Root: "root", Kind: "grep", Query: "needle"})
	if err != nil || len(found.Matches) != 1 || found.Matches[0].Line != 1 {
		t.Fatalf("grep=%+v err=%v", found, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = m.List(ctx, agent.ListRequest{Root: "root"})
	if err != context.Canceled {
		t.Fatal(err)
	}
	_, err = m.Search(ctx, agent.SearchRequest{Root: "root", Kind: "glob", Query: "*"})
	if err != context.Canceled {
		t.Fatal(err)
	}
	if m.Calls("list") != 2 || m.Calls("search") != 3 {
		t.Fatal("actual port calls not counted")
	}
}
