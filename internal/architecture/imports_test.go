package architecture

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportDirection(t *testing.T) {
	root := filepath.Join("..", "..")
	forbidden := map[string][]string{
		"model":           {"/agent", "/session", "/internal/storage"},
		"agent":           {"/session", "/internal/storage"},
		"session/history": {"/agent/eino", "/internal/storage"},
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for prefix, bans := range forbidden {
			if !strings.HasPrefix(rel, prefix+"/") && rel != prefix {
				continue
			}
			for _, imp := range file.Imports {
				value := strings.Trim(imp.Path.Value, `"`)
				for _, ban := range bans {
					if value == "github.com/ww1489/seasprak"+ban || strings.HasPrefix(value, "github.com/ww1489/seasprak"+ban+"/") {
						t.Errorf("%s imports %s", rel, value)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
