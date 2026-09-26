package sessions

import (
	"context"
	"reflect"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestExplicitResumeAfterDiskReopenUsesCheckpointAndOriginalReceipt(t *testing.T) {
	for _, afterTool := range []bool{false, true} {
		name := "after-model"
		if afterTool {
			name = "after-tool"
		}
		t.Run(name, func(t *testing.T) {
			f := pausedResumeFixture(t, afterTool, resumeFixtureOptions{Disk: true})
			before := f.manager.View()
			if err := f.s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			model := versionedPauseModel{testkit.NewFake(testkit.Step{Text: "resumed answer"})}
			f.opts.Model = model
			opened, err := OpenAgentSession(t.Context(), f.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close(context.Background())
			if model.Calls() != 0 || !reflect.DeepEqual(before, opened.rt.manager.View()) {
				t.Fatal("Open ran work or changed safely paused state")
			}
			snap, err := opened.Snapshot(t.Context())
			if err != nil || !snap.Resume[f.input.TraceID].CanResume {
				t.Fatalf("reopen resume eligibility=%+v err=%v", snap.Resume, err)
			}
			cmd := ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq, IdempotencyKey: "resume-key"}
			receipt, err := opened.Resume(t.Context(), cmd)
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return terminal(opened.rt.manager.View().Traces[f.input.TraceID].State) })
			after := opened.rt.manager.View()
			if after.Traces[f.input.TraceID].State != "completed" || model.Calls() != 1 || f.model.Calls() != 1 || f.runs.Load() != 1 {
				t.Fatalf("reopen trace=%+v models=%d/%d tools=%d", after.Traces[f.input.TraceID], f.model.Calls(), model.Calls(), f.runs.Load())
			}
			again, err := opened.Resume(t.Context(), cmd)
			if err != nil || again != receipt || opened.rt.manager.View().LastSeq != after.LastSeq {
				t.Fatalf("lost response retry=%+v want=%+v err=%v", again, receipt, err)
			}
			cmd.ExpectedRevision++
			_, err = opened.Resume(t.Context(), cmd)
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIdempotencyConflict {
				t.Fatalf("changed retry=%v", err)
			}
			status, err := opened.GetOperation(t.Context(), receipt.OperationID)
			if err != nil || status.State != "completed" {
				t.Fatalf("operation=%+v err=%v", status, err)
			}
			if err := opened.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenAgentSession(t.Context(), f.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close(context.Background())
			cmd.ExpectedRevision--
			again, err = reopened.Resume(t.Context(), cmd)
			if err != nil || again != receipt || model.Calls() != 1 || f.runs.Load() != 1 {
				t.Fatalf("reopened idempotency receipt=%+v err=%v calls=%d/%d", again, err, model.Calls(), f.runs.Load())
			}
		})
	}
}

func TestResumeRejectsNewHistoryAfterCheckpoint(t *testing.T) {
	f := pausedResumeFixture(t, false)
	message := agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindUser, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceHuman}, Scope: agent.MessageScope{SessionID: f.opts.SessionID}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.UserInputText{Text: "new history"})}}}
	if err := f.manager.AppendMessage(t.Context(), message); err != nil {
		t.Fatal(err)
	}
	before := f.manager.View()
	_, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleResume || !reflect.DeepEqual(before, f.manager.View()) || f.model.Calls() != 1 || f.runs.Load() != 0 {
		t.Fatalf("stale checkpoint resume=%v", err)
	}
}

func TestResumePreservesAcceptedPendingFollowUp(t *testing.T) {
	f := pausedResumeFixture(t, false, resumeFixtureOptions{PendingFollow: true})
	before := f.manager.View()
	followID := before.Follow[0]
	_, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	view := f.manager.View()
	if view.Inputs[followID].ID != followID || view.Inputs[followID].State != "consumed" || f.runs.Load() != 1 || f.model.Calls() != 3 {
		t.Fatalf("pending input=%+v models=%d tools=%d", view.Inputs[followID], f.model.Calls(), f.runs.Load())
	}
}
