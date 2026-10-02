package architecture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApprovedLayerDirectories(t *testing.T) {
	root := repoRoot()
	for _, name := range []string{"internal/codeagent", "internal/codeagent/state", "internal/storage", "internal/storage/jsonl", "internal/storage/memory"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || !info.IsDir() {
			t.Errorf("required layer directory %s is unavailable: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "internal", "sessions")); !os.IsNotExist(err) {
		t.Errorf("legacy internal/sessions directory still exists: %v", err)
	}
}

func TestImportCheckerRejectsDualAgentBoundarySamples(t *testing.T) {
	cases := []struct {
		from string
		to   string
	}{
		{"internal/codeagent", "internal/workflowagent"},
		{"internal/workflowagent", "internal/codeagent"},
		{"internal/codeagent", "internal/web"},
		{"internal/workflowagent", "internal/web"},
		{"internal/agent", "internal/codeagent"},
		{"internal/agent/eino", "internal/workflowagent"},
		{"internal/llm", "internal/codeagent"},
		{"internal/llm", "internal/workflowagent"},
		{"internal/storage", "internal/codeagent/state"},
		{"internal/storage/jsonl", "internal/workflowagent"},
		{"internal/codeagent/state", "internal/storage/jsonl"},
		{"internal/codeagent/state", "internal/storage/memory"},
		{"internal/codeagent/state", "internal/agent/eino"},
		{"internal/web", "internal/codeagent/state"},
		{"internal/web", "internal/storage/jsonl"},
		{"internal/web", "internal/storage/memory"},
	}
	for _, tc := range cases {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, filepath.FromSlash(tc.from), "bad.go")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			importPath := "github.com/ww1489/seasprak/" + tc.to
			source := "package sample\n\nimport _ \"" + importPath + "\"\n"
			if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			hits := forbiddenImports(root, importRules())
			if len(hits) == 0 {
				t.Fatalf("checker accepted forbidden %s -> %s import", tc.from, tc.to)
			}
			if !strings.Contains(strings.Join(hits, "\n"), importPath) {
				t.Fatalf("checker reported unrelated imports: %v", hits)
			}
		})
	}
}
