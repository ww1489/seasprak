package codeagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

// Controlled time and mailbox availability verify that an expired reservation
// fails closed during approval checkpointing, while timely renewal permits pause.
// This proves the deadline behavior, not the cause of any real-time scheduling delay.
func TestApprovalActivityReservationDeadline(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "renewal_before_deadline"
		if expired {
			name = "mailbox_unavailable_at_deadline"
		}
		t.Run(name, func(t *testing.T) {
			id := agent.MustID()
			backend, err := memory.Open(id, store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			blocked := &blockedApprovalBlobStore{Store: backend, CheckpointBlobs: backend, entered: entered, release: release}
			manager, err := state.NewManager(blocked, id)
			if err != nil {
				t.Fatal(err)
			}
			model := versionedPauseModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "finished"})}
			var runs atomic.Int32
			opts := Options{SessionID: id, Workspace: t.TempDir(), Principal: "host-user", Profile: ProfileMemory, GenerationFingerprint: "approval-bundle-v1", Store: blocked, Model: model, Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "one-operation"}, Run: func(context.Context, json.RawMessage) (string, error) {
				runs.Add(1)
				return "approved result", nil
			}}}}
			if _, err := alignTools(&opts); err != nil {
				t.Fatal(err)
			}
			s, err := Start(opts, manager, "gen")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			clock := newManualActivityClock()
			if err := s.rt.do(t.Context(), func(rt *runtime) error { rt.clock = clock; return nil }); err != nil {
				t.Fatal(err)
			}
			input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool {
				select {
				case <-entered:
					return true
				default:
					return false
				}
			})
			frame := activityFrame(t, s)
			clock.advance(500 * time.Millisecond)
			waitResumeCondition(t, func() bool { return manager.View().Traces[input.TraceID].Activity.Revision == 2 })
			if err := s.rt.do(t.Context(), func(*runtime) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if expired {
				mailboxEntered, mailboxRelease := make(chan struct{}), make(chan struct{})
				mailboxDone := make(chan error, 1)
				t.Cleanup(func() {
					select {
					case <-mailboxRelease:
					default:
						close(mailboxRelease)
					}
				})
				go func() {
					mailboxDone <- s.rt.do(t.Context(), func(*runtime) error {
						close(mailboxEntered)
						<-mailboxRelease
						return nil
					})
				}()
				<-mailboxEntered
				clock.advance(1358707300 * time.Nanosecond)
				if frame.ctx.Err() == nil {
					t.Fatal("expired reservation did not cancel outside the mailbox")
				}
				select {
				case <-frame.done:
					t.Fatal("expiry invented execution exit")
				default:
				}
				close(mailboxRelease)
				if err := <-mailboxDone; err != nil {
					t.Fatal(err)
				}
			} else {
				clock.advance(358707300 * time.Nanosecond)
			}
			close(release)
			waitResumeCondition(t, func() bool {
				tr := manager.View().Traces[input.TraceID]
				return tr.State == "paused" || terminal(tr.State)
			})
			tr := manager.View().Traces[input.TraceID]
			if !tr.ExecutionStopped || tr.Activity.Reserved != 0 || tr.Activity.Revision != 3 || model.Calls() != 1 || runs.Load() != 0 || tr.Usage.TransportRequests != 1 || tr.Usage.ToolExecutions != 0 {
				t.Fatal("approval deadline changed safety counters or settlement")
			}
			if expired {
				if tr.State != "failed" || tr.Activity.Settled != 1858707300*time.Nanosecond || !strings.Contains(tr.Error, product.CodeBudgetExhausted) || tr.CheckpointID != "" {
					t.Fatal("expired lease was not safely rejected")
				}
			} else if tr.State != "paused" || tr.CheckpointID == "" || tr.Activity.Settled != 858707300*time.Nanosecond {
				t.Fatal("timely lease did not retain answerable approval")
			}
		})
	}
}
