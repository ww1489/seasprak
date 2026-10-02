package codeagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

// captureModel records the user texts of every request it receives.
type captureModel struct {
	*testkit.FakeModel
	seen [][]string
}

func (c *captureModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	var texts []string
	for _, m := range in {
		for _, b := range m.ContentBlocks {
			if b != nil && b.UserInputText != nil {
				texts = append(texts, b.UserInputText.Text)
			}
		}
	}
	c.seen = append(c.seen, texts)
	return c.FakeModel.Generate(ctx, in, opts...)
}

func (c *captureModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := c.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

func prompt(t *testing.T, s *AgentSession, text string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"text": text})
	in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: raw})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
}

func TestBranchesIsolateModelContextAndSurviveReopen(t *testing.T) {
	capture := &captureModel{FakeModel: testkit.NewFake()}
	opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "branches", Profile: ProfileMemory, Model: capture, Principal: "local", GenerationFingerprint: "branch-v1"}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	prompt(t, s, "alpha")
	forkPoint := s.rt.manager.View().LeafID
	prompt(t, s, "beta-main")
	mainLeaf := s.rt.manager.View().LeafID
	if err = s.ForkBranch(t.Context(), "side", forkPoint); err != nil {
		t.Fatal(err)
	}
	prompt(t, s, "gamma-side")
	last := strings.Join(capture.seen[len(capture.seen)-1], ",")
	if !strings.Contains(last, "alpha") || !strings.Contains(last, "gamma-side") || strings.Contains(last, "beta-main") {
		t.Fatalf("side request crossed branches: %s", last)
	}
	if err = s.ForkBranch(t.Context(), "side", ""); err == nil {
		t.Fatal("duplicate branch accepted")
	}
	if err = s.ForkBranch(t.Context(), "bad", "not-an-entry"); err == nil {
		t.Fatal("fork from missing entry accepted")
	}
	before := s.rt.manager.View().LastSeq
	if err = s.NavigateBranch(t.Context(), "missing"); err == nil || s.rt.manager.View().LastSeq != before {
		t.Fatal("navigating to a missing branch changed state")
	}
	if err = s.NavigateBranch(t.Context(), "main"); err != nil {
		t.Fatal(err)
	}
	if s.rt.manager.View().LeafID != mainLeaf {
		t.Fatal("main head lost")
	}
	branches, _ := s.ListBranches(t.Context())
	if len(branches) != 2 || !branches[0].Active || branches[0].BranchID != "main" {
		t.Fatalf("branches=%+v", branches)
	}
	page, err := s.ListMessages(t.Context(), "", 1)
	if err != nil || len(page.Messages) != 1 || page.Next == "" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	next, err := s.ListMessages(t.Context(), page.Next, 50)
	if err != nil || len(next.Messages) == 0 || next.Messages[0].ID == page.Messages[0].ID {
		t.Fatal("stable paging repeated an entry")
	}
	mainPath := s.rt.manager.View().Messages
	if err = s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err = OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(t.Context())
	v := s.rt.manager.View()
	if v.BranchID != "main" || v.LeafID != mainLeaf || len(v.Messages) != len(mainPath) {
		t.Fatalf("reopen changed the selected path: %s %s %d", v.BranchID, v.LeafID, len(v.Messages))
	}
	for i := range mainPath {
		if v.Messages[i].ID != mainPath[i].ID {
			t.Fatal("reopened path differs")
		}
	}
	prompt(t, s, "delta-main")
	last = strings.Join(capture.seen[len(capture.seen)-1], ",")
	if strings.Contains(last, "gamma-side") || !strings.Contains(last, "beta-main") {
		t.Fatalf("main request crossed branches after reopen: %s", last)
	}
}

func TestBranchChangesRejectedWhileBusy(t *testing.T) {
	gate := make(chan struct{})
	s, manager, model := controlSession(t, "branch-busy", testkit.Step{Gate: gate})
	in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return model.Calls() == 1 })
	before := manager.View().LastSeq
	err = s.ForkBranch(t.Context(), "side", "")
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict || manager.View().LastSeq != before {
		t.Fatalf("busy fork: %v", err)
	}
	close(gate)
	waitFor(t, func() bool { return manager.View().Traces[in.TraceID].Settled })
}
