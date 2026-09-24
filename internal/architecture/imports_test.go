package architecture

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func importRules() map[string][]string {
	return map[string][]string{
		"internal/errors":                {"/internal/agent", "/internal/sessions", "/internal/llm", "/internal/config", "/sdk", "/cmd"},
		"internal/config":                {"/internal/agent", "/internal/sessions", "/internal/llm", "/internal/errors", "/sdk", "/cmd"},
		"internal/llm":                   {"/internal/agent", "/internal/sessions", "/sdk", "/cmd"},
		"internal/agent":                 {"/internal/sessions", "/sdk", "/cmd"},
		"internal/sessions/state":        {"/internal/sessions/store/jsonl", "/internal/sessions/store/memory", "/internal/agent/eino", "/sdk", "/cmd"},
		"internal/sessions/store":        {"/internal/sessions/state", "/internal/agent/eino", "/sdk", "/cmd"},
		"internal/sessions/store/jsonl":  {"/internal/sessions/state", "/sdk", "/cmd"},
		"internal/sessions/store/memory": {"/internal/sessions/state", "/sdk", "/cmd"},
		"internal":                       {"/sdk", "/cmd"},
		"cmd/agentd":                     {"/sdk", "/internal"},
	}
}

func TestImportDirection(t *testing.T) {
	for _, hit := range forbiddenImports(repoRoot(), importRules()) {
		t.Error(hit)
	}
}

func forbiddenImports(root string, forbidden map[string][]string) []string {
	var hits []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "testdata" || base == ".git" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			hits = append(hits, err.Error())
			return nil
		}
		for prefix, bans := range forbidden {
			if !strings.HasPrefix(rel, prefix+"/") && rel != prefix {
				continue
			}
			for _, imp := range file.Imports {
				value := strings.Trim(imp.Path.Value, `"`)
				for _, ban := range bans {
					if value == "github.com/ww1489/seasprak"+ban || strings.HasPrefix(value, "github.com/ww1489/seasprak"+ban+"/") {
						hits = append(hits, rel+" imports "+value)
					}
				}
			}
		}
		return nil
	})
	return hits
}
