package tools

import (
	"encoding/json"
	product "github.com/ww1489/seasprak/internal/errors"
	"testing"
)

func TestCompileSchemasUsesDeclaredDraftAndRejectsUnregisteredReferences(t *testing.T) {
	for _, tc := range []struct {
		name, schema string
		valid        bool
	}{
		{"default-2020", `{"type":"array","prefixItems":[{"type":"integer"}],"items":false}`, true},
		{"explicit-draft7", `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object"}`, true},
		{"invalid-type", `{"type":"not-a-json-schema-type"}`, false},
		{"unregistered-ref", `{"$ref":"https://example.invalid/remote.schema.json"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schemas, err := CompileSchemas([]Definition{{Name: "work", Schema: json.RawMessage(tc.schema)}})
			if tc.valid {
				if err != nil || schemas["work"] == nil {
					t.Fatalf("compile failed: %v", err)
				}
			} else if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
				t.Fatalf("invalid schema accepted: %v", err)
			}
		})
	}
}
