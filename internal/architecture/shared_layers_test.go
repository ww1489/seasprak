package architecture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportCheckerRejectsSharedLayerSamples(t *testing.T) {
	cases := []struct {
		from string
		to   string
	}{
		{"internal/errors", "internal/codeagent"},
		{"internal/errors", "internal/workflowagent"},
		{"internal/config", "internal/codeagent"},
		{"internal/config", "internal/workflowagent"},
		{"internal/storage", "internal/agent/eino"},
		{"internal/storage/memory", "internal/agent/eino"},
		{"internal/codeagent", "sdk"},
		{"internal/workflowagent", "cmd/web"},
		{"internal/workflowagent/state", "internal/codeagent/state"},
		{"internal/codeagent/state", "internal/workflowagent"},
	}
	for _, tc := range cases {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, filepath.FromSlash(tc.from), "bad.go")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			importPath := "github.com/ww1489/seasprak/" + tc.to
			if err := os.WriteFile(path, []byte("package sample\n\nimport _ \""+importPath+"\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			hits := forbiddenImports(root, importRules())
			if !strings.Contains(strings.Join(hits, "\n"), importPath) {
				t.Fatalf("checker accepted forbidden %s -> %s import: %v", tc.from, tc.to, hits)
			}
		})
	}
}
