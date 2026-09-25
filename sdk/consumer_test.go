package sdk_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIndependentConsumerUsesOnlySDK(t *testing.T) {
	root := repoRoot(t)
	sources, err := filepath.Glob(filepath.Join(root, "sdk", "testdata", "consumer", "*_test.go"))
	if err != nil || len(sources) == 0 {
		t.Fatalf("consumer test sources: %v", err)
	}
	dir := t.TempDir()
	for _, path := range sources {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(path)), src, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConsumerModule(t, dir, root)
	cmd := exec.Command("go", "test", "-count=1", "-timeout", "60s")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=-mod=readonly")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("independent consumer failed: %v\n%s", err, out)
	}
}

func writeConsumerModule(t *testing.T, dir, root string) {
	t.Helper()
	// Reuse the locked graph, not just the direct imports. A fresh module can
	// otherwise need uncached transitive go.mod files even after the SDK builds.
	for _, name := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Let Go quote the replacement path, including spaces, on every platform.
	cmd := exec.Command("go", "mod", "edit", "-module=consumer",
		"-require=github.com/ww1489/seasprak@v0.0.0",
		"-replace=github.com/ww1489/seasprak="+filepath.ToSlash(root))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prepare consumer module: %v\n%s", err, out)
	}
}

func TestConsumerModulePreservesDependencyPinsAndPaths(t *testing.T) {
	for _, name := range []string{"module", "module with spaces"} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), name)
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			mod := []byte("module github.com/ww1489/seasprak\n\ngo 1.27.0\n\nrequire example.com/pinned v1.2.3 // indirect\n")
			sum := []byte("example.com/pinned v1.2.3 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n")
			for name, data := range map[string][]byte{"go.mod": mod, "go.sum": sum} {
				if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			writeConsumerModule(t, dir, root)
			cmd := exec.Command("go", "mod", "edit", "-json")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("parse consumer module: %v\n%s", err, out)
			}
			var got struct {
				Module  struct{ Path string }
				Require []struct {
					Path, Version string
					Indirect      bool
				}
				Replace []struct{ Old, New struct{ Path string } }
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if got.Module.Path != "consumer" {
				t.Fatalf("module = %q", got.Module.Path)
			}
			pins := map[string]string{}
			for _, dep := range got.Require {
				pins[dep.Path] = dep.Version
				if dep.Path == "example.com/pinned" && !dep.Indirect {
					t.Error("indirect dependency marker was lost")
				}
			}
			if pins["example.com/pinned"] != "v1.2.3" || pins["github.com/ww1489/seasprak"] != "v0.0.0" {
				t.Errorf("dependency pins = %v", pins)
			}
			if len(got.Replace) != 1 || got.Replace[0].Old.Path != "github.com/ww1489/seasprak" || got.Replace[0].New.Path != filepath.ToSlash(root) {
				t.Errorf("replacement = %+v", got.Replace)
			}
			copiedSum, err := os.ReadFile(filepath.Join(dir, "go.sum"))
			if err != nil || !bytes.Equal(copiedSum, sum) {
				t.Errorf("consumer checksums were not preserved: %v", err)
			}
			for name, want := range map[string][]byte{"go.mod": mod, "go.sum": sum} {
				got, err := os.ReadFile(filepath.Join(root, name))
				if err != nil || !bytes.Equal(got, want) {
					t.Errorf("source %s was modified: %v", name, err)
				}
			}
		})
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), ".."))
}
