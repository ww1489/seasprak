package consumer_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/sdk"
)

func TestSDKConsumerCanRegisterReconcileQuery(t *testing.T) {
	var query sdk.ReconcileQuery = sdk.ReconcileQueryFunc(func(context.Context, sdk.ReconcileQueryRequest) (sdk.ReconcileEvidence, error) {
		return sdk.ReconcileEvidence{EvidenceRefs: []string{"evidence"}, EvidenceSource: "consumer"}, nil
	})
	if query == nil {
		t.Fatal("reconcile query registration returned nil")
	}
	raw, err := json.Marshal(sdk.ReconcileEvidence{EvidenceRefs: []string{"evidence"}, EvidenceSource: "consumer", TrustedNoStart: true})
	if err != nil || !strings.Contains(string(raw), `"evidenceRefs"`) || !strings.Contains(string(raw), `"trustedNoStart":true`) {
		t.Fatalf("reconcile evidence is not a public lowerCamelCase DTO: %s err=%v", raw, err)
	}
}
