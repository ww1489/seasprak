package sessions

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestExecuteCommandPreservesOrdinaryJSONArguments(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		invalid   bool
	}{
		{"array-order-and-number", `{"argv":["echo"],"required":["z","a"],"n":9007199254740993}`, false},
		{"trailing-value", `{"argv":["echo"]} {}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			process := &commandProcessProbe{}
			s, err := CreateAgentSession(t.Context(), Options{Workspace: t.TempDir(), Profile: ProfileMemory, StateRoot: "memory", Model: testkit.NewFake(), Tools: []tools.Definition{builtinDefinitionForSession(t, "execute")}, Operations: tools.Operations{Process: process}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			_, err = s.ExecuteCommand(t.Context(), CommandRequest{Name: "execute", Arguments: json.RawMessage(tc.raw)})
			if tc.invalid {
				if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
					t.Fatalf("error=%v", err)
				}
				if len(s.rt.manager.View().Operations) != 0 || process.calls.Load() != 0 {
					t.Fatal("invalid JSON was accepted or executed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			view := s.rt.manager.View()
			for _, input := range view.Inputs {
				var envelope commandEnvelope
				if err := json.Unmarshal(input.Content, &envelope); err != nil {
					t.Fatal(err)
				}
				want := `{"argv":["echo"],"n":9007199254740993,"required":["z","a"]}`
				if string(envelope.Arguments) != want {
					t.Fatalf("arguments=%s want=%s", envelope.Arguments, want)
				}
				return
			}
			t.Fatal("command input missing")
		})
	}
}
