package state_test

import (
	"reflect"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func TestApprovalLegacyPublicWritersAreBlocked(t *testing.T) {
	for _, kind := range []string{"request", "approval", "response"} {
		t.Run(kind, func(t *testing.T) {
			m, _ := fixture(t)
			before := m.View()
			var err error
			switch kind {
			case "request":
				err = m.SaveRecords(t.Context(), before.LastSeq, state.Records{Interactions: []state.Interaction{{ID: "legacy", Kind: "approval"}}})
			case "approval":
				err = m.SaveRecords(t.Context(), before.LastSeq, state.Records{Approvals: []state.Approval{{ID: "legacy"}}})
			case "response":
				_, err = m.AcceptOperation(t.Context(), state.OperationCommand{Kind: "respond_interaction", Target: "legacy", ExpectedRevision: before.LastSeq})
			}
			requireP2Code(t, err, product.CodePermissionDenied)
			if !reflect.DeepEqual(before, m.View()) {
				t.Fatal("legacy approval writer changed durable state")
			}
		})
	}
}
