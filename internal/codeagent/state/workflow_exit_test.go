package state_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
)

func TestValidateCodeCommitCompatibilityPreservesInput(t *testing.T) {
	for _, tc := range []struct {
		name, kind, payload string
		rejected            bool
	}{
		{"ordinary", "model_attempt", `{"purpose":"agent"}`, false},
		{"compaction", "model_attempt", `{"purpose":"compaction"}`, false},
		{"ordinary_frozen", "frozen_execution", `{"origin":"model"}`, false},
		{"node", "workflow_node", `{}`, true},
		{"purpose", "model_attempt", `{"purpose":"workflow_node"}`, true},
		{"origin", "frozen_execution", `{"origin":"workflow_node"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commit := storage.Commit{ControlRecords: []storage.Record{{Type: tc.kind, Version: 1, ID: "fact", Payload: json.RawMessage(tc.payload)}}}
			before := storage.CloneCommit(commit)
			err := state.ValidateCodeCommitCompatibility(commit)
			if tc.rejected {
				requireP2Code(t, err, product.CodeIncompatibleVersion)
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(commit, before) {
				t.Fatal("compatibility check mutated its input")
			}
		})
	}
}

func TestCodeStateRejectsExitedWorkflowPurposeAndOriginBeforeAppend(t *testing.T) {
	for _, kind := range []string{"purpose", "origin"} {
		t.Run(kind, func(t *testing.T) {
			m, backend := fixture(t)
			before := m.View()
			stored, err := backend.Load(t.Context(), "session")
			if err != nil {
				t.Fatal(err)
			}
			records := state.Records{}
			if kind == "purpose" {
				records.ModelAttempts = []state.ModelAttempt{{ID: "legacy", Purpose: "workflow_node", State: "started"}}
			} else {
				records.FrozenExecutions = []state.FrozenExecution{{ID: "legacy", Origin: "workflow_node"}}
			}
			err = m.SaveRecords(t.Context(), before.LastSeq, records)
			requireP2Code(t, err, product.CodeIncompatibleVersion)
			after, err := backend.Load(t.Context(), "session")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, m.View()) || !reflect.DeepEqual(stored, after) || m.Fault() != nil {
				t.Fatal("exited workflow fact was appended or changed projection")
			}
		})
	}
}
