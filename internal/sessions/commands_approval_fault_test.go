package sessions

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type commandApprovalFaultStore struct {
	store.Store
	target atomic.Value
	lost   bool
}

func (s *commandApprovalFaultStore) Close() error { return nil }
func (s *commandApprovalFaultStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	target, _ := s.target.Load().(string)
	for _, r := range c.ControlRecords {
		match := r.Type == target
		if target == "result" && r.Type == "operation" {
			var op state.Operation
			_ = json.Unmarshal(r.Payload, &op)
			match = op.Kind == "direct_command" && op.State == "completed"
		}
		if match {
			if s.lost {
				if _, err := s.Store.Append(ctx, id, expected, c); err != nil {
					return store.CommitReceipt{}, err
				}
			}
			return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "injected command approval append failure")
		}
	}
	return s.Store.Append(ctx, id, expected, c)
}
func TestCommandApprovalAppendFailuresAndLostAcknowledgements(t *testing.T) {
	for _, target := range []string{"direct_resume", "approval_decision", "resumed_execution", "approval_claim", "result"} {
		for _, lost := range []bool{false, true} {
			name := target
			if lost {
				name += "-lost-ack"
			}
			t.Run(name, func(t *testing.T) {
				id := agent.MustID()
				backend, err := memory.Open(id, store.Header{})
				if err != nil {
					t.Fatal(err)
				}
				defer backend.Close()
				faults := &commandApprovalFaultStore{Store: backend, lost: lost}
				if target == "direct_resume" {
					faults.target.Store(target)
				}
				process := &commandProcessProbe{}
				def := builtinDefinitionForSession(t, "execute")
				def.Execution.RequestedGrantRef = "requires-approval"
				opts := Options{SessionID: id, Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Principal: "host", GenerationFingerprint: "approval-fault-v1", Model: testkit.NewFake(), Store: faults, Tools: []tools.Definition{def}, Operations: tools.Operations{Process: process}, ResourceScheduler: tools.NewResourceScheduler()}
				s, err := CreateAgentSession(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close(context.Background())
				receipt, err := s.ExecuteCommand(t.Context(), atomicCommandRequest())
				if err != nil {
					t.Fatal(err)
				}
				waitResumeCondition(t, func() bool { return s.rt.manager.Fault() != nil || len(s.rt.manager.View().DirectResumes) == 1 })
				trace := ""
				for id := range s.rt.manager.View().Traces {
					trace = id
				}
				if target != "direct_resume" {
					if target == "approval_decision" {
						faults.target.Store(target)
					}
					v := s.rt.manager.View()
					var inID string
					for id := range v.Interactions {
						inID = id
					}
					_, err = s.RespondInteraction(t.Context(), InteractionResponse{InteractionID: inID, Decision: "allowed-once", ExpectedRevision: v.LastSeq, IdempotencyKey: "answer"})
					if target == "approval_decision" {
						requireSessionCode(t, err, product.CodeStorageUnavailable)
					} else {
						if err != nil {
							t.Fatal(err)
						}
						faults.target.Store(target)
						_, err = s.Resume(t.Context(), ResumeCommand{TraceID: trace, ExpectedRevision: s.rt.manager.View().LastSeq, IdempotencyKey: "resume"})
						if target == "resumed_execution" {
							requireSessionCode(t, err, product.CodeStorageUnavailable)
						} else if err != nil {
							t.Fatal(err)
						}
					}
				}
				waitResumeCondition(t, func() bool { return s.rt.manager.Fault() != nil })
				if err := s.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
				want := int32(0)
				if target == "result" {
					want = 1
				}
				if process.calls.Load() != want {
					t.Fatalf("failure backend=%d want=%d", process.calls.Load(), want)
				}
				// Rebuild solely from accepted journal lines, including an acknowledgement
				// lost after append. Reopening never restarts a worker.
				faults.target.Store("")
				manager, err := state.NewManager(faults, id)
				if err != nil {
					t.Fatal(err)
				}
				opened, err := Start(s.rt.opts, manager, s.rt.generation)
				if err != nil {
					t.Fatal(err)
				}
				defer opened.Close(context.Background())
				v := manager.View()
				if process.calls.Load() != want {
					t.Fatal("reopen executed backend")
				}
				switch target {
				case "direct_resume":
					if (len(v.DirectResumes) == 1) != lost {
						t.Fatal("waiting append visibility differs")
					}
					if lost {
						answerCommand(t, opened, "allowed-once")
						_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: trace, ExpectedRevision: manager.View().LastSeq})
						if err != nil {
							t.Fatal(err)
						}
						waitResumeCondition(t, func() bool { return terminal(manager.View().Traces[trace].State) })
						if process.calls.Load() != 1 {
							t.Fatal("durable wait failed to resume once")
						}
					}
				case "approval_decision":
					if (len(v.ApprovalDecisions) == 1) != lost {
						t.Fatal("decision append visibility differs")
					}
				case "resumed_execution":
					if (len(v.ResumedExecutions) == 1) != lost {
						t.Fatal("resume append visibility differs")
					}
					if lost {
						_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: trace, ExpectedRevision: v.LastSeq})
						requireSessionCode(t, err, product.CodeIncompatibleResume)
					}
				case "approval_claim":
					if (len(v.ApprovalClaims) == 1) != lost {
						t.Fatal("claim append visibility differs")
					}
					if lost && !v.HasUnresolvedEffects() {
						t.Fatal("lost claim was not retained as unknown")
					}
					_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: trace, ExpectedRevision: v.LastSeq})
					if err == nil {
						t.Fatal("interrupted claim resumed")
					}
				case "result":
					if (v.Operations[receipt.OperationID].State == "completed") != lost {
						t.Fatal("command completion append visibility differs")
					}
					call := v.Calls[receipt.OperationID]
					if call.Observation == nil || !call.Claimed {
						t.Fatal("executed result disappeared")
					}
					_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: trace, ExpectedRevision: v.LastSeq})
					if err == nil || process.calls.Load() != 1 {
						t.Fatal("saved observation was executed again")
					}
				}
			})
		}
	}
}
