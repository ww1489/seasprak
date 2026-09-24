package architecture

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSDKHasSingleProductionFile(t *testing.T) {
	root := repoRoot()
	var prod []string
	err := filepath.Walk(filepath.Join(root, "sdk"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(filepath.Join(root, "sdk"), path)
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			if rel == "testdata" || strings.HasPrefix(rel, "testdata/") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			prod = append(prod, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prod) != 1 || prod[0] != "sdk.go" {
		t.Fatalf("sdk production files = %v", prod)
	}
}

func TestLegacyPublicPathsAreGone(t *testing.T) {
	root := repoRoot()
	for _, name := range []string{"sdk.go", "agent", "session", "extensions", "model"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("legacy path %s still exists: %v", name, err)
		}
	}
}

func TestCmdAgentdImportsOnlyStdlib(t *testing.T) {
	root := repoRoot()
	dir := filepath.Join(root, "cmd", "agentd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"flag": true, "fmt": true, "io": true, "os": true}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			value := strings.Trim(imp.Path.Value, `"`)
			if !allowed[value] {
				t.Errorf("cmd/agentd imports %s", value)
			}
		}
	}
}

func TestImportCheckerRejectsForbiddenSample(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "internal", "agent", "bad.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	src := "package agent\n\nimport _ \"github.com/ww1489/seasprak/internal/sessions\"\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	hits := forbiddenImports(dir, importRules())
	if len(hits) == 0 {
		t.Fatal("checker accepted a forbidden internal/agent -> sessions import")
	}
}

func repoRoot() string {
	return filepath.Join("..", "..")
}
