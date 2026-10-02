package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/codeagent"
)

// Pending reconciliations use the reconcile request field names, so the page
// can submit them unchanged; an empty list encodes as [].
func TestSnapshotProjectsPendingReconciliations(t *testing.T) {
	snap := emptySnapshot(3)
	if out := projectSnapshot(snap); out.PendingReconciliations == nil || len(out.PendingReconciliations) != 0 {
		t.Fatalf("empty pending must encode as [] got %#v", out.PendingReconciliations)
	}
	snap.PendingReconciliations = []codeagent.PendingReconciliation{{TraceID: "t1", InvocationID: "inv", CallID: "call", ObservationID: "obs", ObservationVersion: 2}}
	raw, err := json.Marshal(projectSnapshot(snap))
	if err != nil {
		t.Fatal(err)
	}
	want := `"pendingReconciliations":[{"traceId":"t1","invocationId":"inv","toolCallId":"call","observationId":"obs","observationVersion":2}]`
	if !strings.Contains(string(raw), want) {
		t.Fatalf("snapshot %s", raw)
	}
}
