package storage

import (
	"encoding/json"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
)

func TestCreationRegistryTypedRoots(t *testing.T) {
	r, err := OpenCreationRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var workflow CreationRecord
	if err := json.Unmarshal([]byte(`{"resourceType":"workflow","runId":"same-id","digest":"wf","generation":"v1"}`), &workflow); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(t.Context(), "workflow", workflow); err != nil {
		t.Fatalf("workflow reservation rejected: %v", err)
	}
	code := CreationRecord{SessionID: "same-id", Digest: "code", Generation: "v1"}
	if err := r.Save(t.Context(), "code", code); err != nil {
		t.Fatal(err)
	}
	workflow.Receipt = json.RawMessage(`{"runId":"same-id","state":"accepted"}`)
	if err := r.Save(t.Context(), "workflow", workflow); err != nil {
		t.Fatal(err)
	}
	got, found, err := r.Load(t.Context(), "workflow")
	raw, _ := json.Marshal(got)
	if err != nil || !found || string(raw) != `{"resourceType":"workflow","runId":"same-id","digest":"wf","generation":"v1","receipt":{"runId":"same-id","state":"accepted"}}` {
		t.Fatalf("typed root lost: %s err=%v", raw, err)
	}
	for _, bad := range []string{
		`{"resourceType":"workflow","sessionId":"same-id","runId":"same-id","digest":"wf","generation":"v1"}`,
		`{"resourceType":"workflow","sessionId":"same-id","digest":"wf","generation":"v1"}`,
		`{"runId":"same-id","digest":"wf","generation":"v1"}`,
		`{"resourceType":"unknown","sessionId":"same-id","digest":"wf","generation":"v1"}`,
		`{"resourceType":"code","runId":"same-id","digest":"wf","generation":"v1"}`,
	} {
		var record CreationRecord
		if err := json.Unmarshal([]byte(bad), &record); err != nil {
			t.Fatal(err)
		}
		if err := r.Save(t.Context(), "bad", record); err == nil {
			t.Fatal("ambiguous or unsupported root accepted")
		} else if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
			t.Fatalf("invalid root error=%v", err)
		}
	}
	var changed CreationRecord
	_ = json.Unmarshal([]byte(`{"resourceType":"code","sessionId":"same-id","digest":"wf","generation":"v1"}`), &changed)
	if err := r.Save(t.Context(), "workflow", changed); err == nil {
		t.Fatal("creation root changed")
	} else if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict {
		t.Fatalf("root mutation error=%v", err)
	}
}
