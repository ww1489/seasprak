package operations_test

import (
	"io"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

func TestOutputArtifactsSaveReadAndBinding(t *testing.T) {
	m := fixture.NewMemory()
	input := agent.OutputArtifactInput{Binding: agent.OutputArtifactBinding{SessionID: "session", Environment: "environment", CallID: "call"}, Content: strings.Repeat("脱敏日志\n", 2100), MediaType: "text/plain", Name: "execute.log"}
	ref, err := m.SaveOutput(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !ref.Available || ref.SessionID != input.Binding.SessionID || ref.Environment != input.Binding.Environment || ref.Hash == "" || ref.Size != int64(len(input.Content)) {
		t.Fatal("incomplete artifact reference")
	}
	reader, err := m.OpenOutput(t.Context(), agent.OutputArtifactRead{Binding: input.Binding, Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(body) != input.Content {
		t.Fatal("full log read mismatch", err)
	}
	for _, field := range []string{"session", "environment", "call", "hash", "size", "unavailable", "missing"} {
		t.Run(field, func(t *testing.T) {
			r := agent.OutputArtifactRead{Binding: input.Binding, Ref: ref}
			want := product.CodePermissionDenied
			switch field {
			case "session":
				r.Binding.SessionID = "other"
			case "environment":
				r.Binding.Environment = "other"
			case "call":
				r.Binding.CallID = "other"
			case "hash":
				r.Ref.Hash = "changed"
				want = product.CodeStateConflict
			case "size":
				r.Ref.Size++
				want = product.CodeStateConflict
			case "unavailable":
				r.Ref.Available = false
				want = product.CodeStateConflict
			case "missing":
				r.Ref.ID = "missing"
				want = product.CodeResourceUnavailable
			}
			reader, err := m.OpenOutput(t.Context(), r)
			if reader != nil {
				_ = reader.Close()
				t.Fatal("invalid read returned content")
			}
			code(t, err, want)
		})
	}
	if m.Calls("save") != 0 || m.Calls("save_output") != 1 {
		t.Fatal("post-processing consumed legacy execution path")
	}
}
