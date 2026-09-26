package sessions

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestReopenRejectsChangedExecutionDeclaration(t *testing.T) {
	definition := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "trusted-run", Effect: "read"}}
	opts := Options{SessionID: "generation-execution", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: testkit.NewFake(), Tools: []tools.Definition{definition}}
	session, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Same tool name/version/schema, but a different backend and effect. This
	// must not silently retain the persisted generation's identity on reopen.
	opts.Tools[0].Execution = tools.ExecutionDescription{BackendID: "process-operations", Effect: "write", Argv: []string{"fixture"}}
	reopened, err := OpenAgentSession(t.Context(), opts)
	if reopened != nil {
		_ = reopened.Close(context.Background())
	}
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeIncompatibleVersion {
		t.Fatalf("reopen accepted changed execution declaration: %v", err)
	}
}

func TestCreateRejectsInvalidToolSchemaBeforeExecution(t *testing.T) {
	definition := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"not-a-json-schema-type"}`)}
	opts := Options{SessionID: "invalid-schema", Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: testkit.NewFake(), Tools: []tools.Definition{definition}}
	session, err := CreateAgentSession(t.Context(), opts)
	if session != nil {
		_ = session.Close(context.Background())
	}
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeInvalidArgument {
		t.Fatalf("invalid generation schema was accepted at creation: %v", err)
	}
}
