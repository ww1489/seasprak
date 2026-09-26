package sessions

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestResumeAuthorizesToolInNewPendingFollowUpSegment(t *testing.T) {
	f := pausedResumeFixture(t, true, resumeFixtureOptions{PendingFollow: true, ToolInFollowUp: true})
	before := f.manager.View()
	if _, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq}); err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	view := f.manager.View()
	if view.Traces[f.input.TraceID].State != "completed" || f.model.Calls() != 4 || f.runs.Load() != 2 || view.Traces[f.input.TraceID].Usage.ToolExecutions != 2 || view.Inputs[before.Follow[0]].State != "consumed" {
		t.Fatalf("follow-up trace=%+v model=%d tools=%d", view.Traces[f.input.TraceID], f.model.Calls(), f.runs.Load())
	}
	for _, call := range view.Calls {
		if call.Observation == nil || call.Observation.Status != "succeeded" {
			t.Fatalf("follow-up call=%+v", call)
		}
	}
}

func TestResumeAuthorizesNewToolTurnAfterOriginalTurn(t *testing.T) {
	for _, onDisk := range []bool{false, true} {
		name := "same-instance"
		if onDisk {
			name = "disk-reopen"
		}
		t.Run(name, func(t *testing.T) {
			f := pausedResumeFixture(t, true, resumeFixtureOptions{AdditionalTool: true, Disk: onDisk})
			s := f.s
			calls := f.model.Calls
			wantCalls := 3
			if onDisk {
				if err := s.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
				model := versionedPauseModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "next-provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "finished"})}
				f.opts.Model = model
				var err error
				s, err = OpenAgentSession(t.Context(), f.opts)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close(context.Background())
				calls, wantCalls = model.Calls, 2
			}
			before := s.rt.manager.View()
			receipt, err := s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[f.input.TraceID].State) })
			view := s.rt.manager.View()
			if view.Traces[f.input.TraceID].State != "completed" || calls() != wantCalls || f.runs.Load() != 2 || view.Traces[f.input.TraceID].Usage.ToolExecutions != 2 {
				t.Fatalf("new tool turn trace=%+v models=%d tools=%d", view.Traces[f.input.TraceID], calls(), f.runs.Load())
			}
			providers := map[string]bool{}
			for _, call := range view.Calls {
				if call.Observation == nil || call.Observation.Status != "succeeded" || !call.Claimed {
					t.Fatalf("new tool denied after resume: %+v", call)
				}
				providers[call.Call.ProviderCallID] = true
			}
			if len(view.Calls) != 2 || !providers["provider"] || !providers["next-provider"] {
				t.Fatalf("call identities=%+v", view.Calls)
			}
			status, err := s.GetOperation(t.Context(), receipt.OperationID)
			if err != nil || status.State != "completed" {
				t.Fatalf("resume operation=%+v err=%v", status, err)
			}
		})
	}
}
