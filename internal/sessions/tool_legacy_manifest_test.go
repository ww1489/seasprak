package sessions

import (
	"context"
	"encoding/json"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
	"os"
	"path/filepath"
	"testing"
)

func TestReopenPreservesLegacyManifestWithoutExecutionField(t *testing.T) {
	def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`)}
	opts := Options{SessionID: "legacy-generation", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: testkit.NewFake(), Tools: []tools.Definition{def}}
	session, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	generation := session.rt.generation
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	decls, err := alignTools(&opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := range decls {
		decls[i].Execution = nil
	}
	legacy, err := buildManifest(generation, decls, opts.GenerationFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveManifest(opts.StateRoot, opts.SessionID, legacy); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(opts.StateRoot, "sessions", opts.SessionID, "resources", generation, "manifest.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("legacy manifest was rewritten")
	}
	opts.Tools[0].Version = "2"
	resumed, err := OpenAgentSession(t.Context(), opts)
	if resumed != nil {
		_ = resumed.Close(context.Background())
	}
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
		t.Fatalf("legacy explicit version change accepted: %v", err)
	}
}
