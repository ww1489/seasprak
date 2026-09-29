package sessions

import (
	"context"
	"errors"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

type activityStatusStore struct {
	store.Store
	repair bool
}

func (s activityStatusStore) Load(context.Context, string) (store.StoredSession, error) {
	return store.StoredSession{RepairRequired: s.repair}, nil
}
func (activityStatusStore) Append(context.Context, string, store.ExpectedCommit, store.Commit) (store.CommitReceipt, error) {
	return store.CommitReceipt{}, errors.New("status test rejected append")
}

func TestActivityWritablePreservesErrorPriority(t *testing.T) {
	for _, storage := range []string{"healthy", "repair", "fault"} {
		for _, access := range []string{"write", "read_only", "closing"} {
			t.Run(storage+"/"+access, func(t *testing.T) {
				m, err := state.NewManager(activityStatusStore{repair: storage == "repair"}, "status")
				if err != nil {
					t.Fatal(err)
				}
				if storage == "fault" {
					_, err = m.Accept(t.Context(), agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"hi"}`)}, agent.TargetAgent{Name: "main", Generation: "gen"})
					if err == nil || m.Fault() == nil {
						t.Fatal("failed to create storage fault")
					}
				}
				rt := &runtime{manager: m, closing: access == "closing", opts: Options{ReadOnly: access != "write"}}
				err = rt.writable()
				wantCode, wantMessage := "", ""
				switch {
				case access == "closing":
					wantCode, wantMessage = product.CodeStateConflict, "session is closing"
				case access == "read_only":
					wantCode, wantMessage = product.CodePermissionDenied, "session is read-only"
				case storage == "fault":
					if err != m.Fault() {
						t.Fatal("fault identity changed")
					}
					return
				case storage == "repair":
					wantCode, wantMessage = product.CodeStorageUnavailable, "journal requires repair"
				default:
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				pe, ok := product.AsError(err)
				if !ok || pe.Code != wantCode || pe.Message != wantMessage {
					t.Fatalf("writable=%v want=%s/%s", err, wantCode, wantMessage)
				}
			})
		}
	}
}

func TestActivityBeginMissingTraceReturnsConflict(t *testing.T) {
	m, err := state.NewManager(activityStatusStore{}, "missing")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	frame := &execution{scope: agent.ExecutionScope{TraceID: "missing", ExecutionID: "execution"}, ctx: ctx, cancel: cancel}
	rt := &runtime{manager: m, active: frame, mailbox: make(chan command), done: make(chan struct{})}
	// Execute exactly the real beginActivity mailbox closure, without starting
	// unrelated session scheduling or a worker for the deliberately absent trace.
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		cmd := <-rt.mailbox
		value, err := cmd.fn(rt)
		cmd.reply <- commandResult{value: value, err: err}
	}()
	err = rt.beginActivity(frame)
	<-joined
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeStateConflict || frame.activity != nil || ctx.Err() != nil || m.View().LastSeq != 0 {
		t.Fatalf("missing trace created activity or wrong error: %v", err)
	}
}
