package sessions

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestLateResumedObservationRequiresDurableOriginalCallMapping(t *testing.T) {
	f := pausedResumeFixture(t, false)
	before := f.manager.View()
	if _, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq}); err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	view := f.manager.View()
	for _, call := range view.Calls {
		body, err := json.Marshal(call)
		if err != nil {
			t.Fatal(err)
		}
		envelope := call.Scope
		envelope.ExecutionID = view.Traces[f.input.TraceID].ExecutionID
		if err := f.s.rt.CommitFact(context.Background(), envelope, agent.Fact{Kind: "tool_observation", Payload: body}); err != nil {
			t.Fatalf("durable resumed observation mapping was lost: %v", err)
		}
		if !reflect.DeepEqual(view, f.manager.View()) {
			t.Fatal("duplicate late observation changed original facts")
		}
		envelope.ExecutionID = agent.MustID()
		err = f.s.rt.CommitFact(context.Background(), envelope, agent.Fact{Kind: "tool_observation", Payload: body})
		if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict || !reflect.DeepEqual(view, f.manager.View()) {
			t.Fatalf("unmapped late callback=%v", err)
		}
	}
}
