package sessions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type p2AttemptFaultStore struct {
	store.Store
	kind     string
	failed   chan struct{}
	rejected store.Commit
}

func (s *p2AttemptFaultStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	for _, record := range c.ControlRecords {
		if record.Type == s.kind {
			s.rejected = c
			close(s.failed)
			return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "synthetic attempt append failure")
		}
	}
	return s.Store.Append(ctx, id, expected, c)
}
func TestP2AttemptAppendFailureNeverStartsToolOrPublishesCandidate(t *testing.T) {
	for _, kind := range []string{"model_attempt", "model_attempt_transition"} {
		t.Run(kind, func(t *testing.T) {
			backend, err := memory.Open("fault-attempt", store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			faults := &p2AttemptFaultStore{Store: backend, kind: kind, failed: make(chan struct{})}
			manager, err := state.NewManager(faults, "fault-attempt")
			if err != nil {
				t.Fatal(err)
			}
			var sent, runs atomic.Int32
			model := &p2AttemptObservedModel{script: testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "write", Arguments: `{}`}}})}
			model.client = &http.Client{Transport: llm.NewObservedTransport(sessionWire(func(r *http.Request) (*http.Response, error) {
				sent.Add(1)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
			}))}
			opts := Options{SessionID: "fault-attempt", Profile: ProfileMemory, Store: faults, Model: llm.WithObservedTransportModel(model), Tools: []tools.Definition{{Name: "write", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "written", nil }}}}
			if _, err := alignTools(&opts); err != nil {
				t.Fatal(err)
			}
			session, err := Start(opts, manager, "gen")
			if err != nil {
				t.Fatal(err)
			}
			_, err = session.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"write"}`)})
			if err != nil {
				t.Fatal(err)
			}
			<-faults.failed
			closeCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if err := session.Close(closeCtx); err != nil {
				t.Fatal(err)
			}
			v := manager.View()
			expectedRequests := int32(0)
			expectedAttempts := 0
			if kind == "model_attempt_transition" {
				expectedRequests = 1
				expectedAttempts = 1
			}
			if sent.Load() != expectedRequests || runs.Load() != 0 || len(v.ModelAttempts) != expectedAttempts || len(v.AttemptResults) != 0 || len(v.Calls) != 0 || len(v.Messages) != 1 || v.LastSeq != faults.rejected.ExpectedPreviousSeq || manager.Fault() == nil {
				t.Fatalf("fault view: sent=%d runs=%d attempts=%d terminals=%d messages=%d calls=%d revision=%d rejectedPrevious=%d", sent.Load(), runs.Load(), len(v.ModelAttempts), len(v.AttemptResults), len(v.Messages), len(v.Calls), v.LastSeq, faults.rejected.ExpectedPreviousSeq)
			}
		})
	}
}
