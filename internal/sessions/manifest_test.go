package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
)

func TestSaveRejectsEscapingIDs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := sampleManifest(t)
	if err := saveManifest(root, "..", manifest); err == nil {
		t.Fatal("session id .. was accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "resources")); !os.IsNotExist(err) {
		t.Fatalf("session id .. wrote outside the session tree: %v", err)
	}
	manifest.ID = ".."
	if err := saveManifest(root, "sess-ok", manifest); err == nil {
		t.Fatal("generation id .. was accepted")
	}
	escaped := filepath.Join(root, "sessions", "sess-ok", "manifest.json")
	if _, err := os.Stat(escaped); !os.IsNotExist(err) {
		t.Fatalf("generation id .. escaped resources: %v", err)
	}
}

func TestLoadRejectsTamperedHash(t *testing.T) {
	root := t.TempDir()
	manifest := sampleManifest(t)
	if err := saveManifest(root, "sess-ok", manifest); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sessions", "sess-ok", "resources", manifest.ID, "manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["tools"] = []any{"other"}
	tampered, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = loadManifest(root, "sess-ok", manifest.ID)
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeIncompatibleVersion {
		t.Fatalf("tampered manifest error = %v", err)
	}
}

func TestBuildFingerprintCoversDeclaration(t *testing.T) {
	base := []toolDecl{{
		Name: "add", Version: "v1", Description: "adds",
		Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
	}}
	left, err := buildManifest("", base, "")
	if err != nil {
		t.Fatal(err)
	}
	right, err := buildManifest("", base, "")
	if err != nil {
		t.Fatal(err)
	}
	if left.Hash != right.Hash || left.ID == "" {
		t.Fatalf("fingerprint not stable: %+v %+v", left, right)
	}
	changed := append([]toolDecl(nil), base...)
	changed[0].Description = "replaced"
	other, err := buildManifest("", changed, "")
	if err != nil {
		t.Fatal(err)
	}
	if other.Hash == left.Hash {
		t.Fatal("description change did not change the fingerprint")
	}
	bare := []toolDecl{{Name: "add", Schema: json.RawMessage(`{"type":"object"}`)}}
	if _, err := buildManifest("", bare, ""); err == nil {
		t.Fatal("empty tool version was accepted")
	}
	builtin, err := buildManifest("", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if builtin.ID != builtinVersion || builtin.Version != builtinVersion {
		t.Fatalf("builtin generation = %+v", builtin)
	}
}

func sampleManifest(t *testing.T) capabilityManifest {
	t.Helper()
	manifest, err := buildManifest("", []toolDecl{{
		Name: "add", Version: "v1", Description: "adds",
		Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
	}}, "")
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestLoadRejectsUnsafeGeneration(t *testing.T) {
	_, err := loadManifest(t.TempDir(), "sess-ok", "a:b")
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeInvalidArgument {
		t.Fatalf("colon generation error = %v", err)
	}
}
