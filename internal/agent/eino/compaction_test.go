package eino

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/testkit"
)

func validSummary() string { return strings.Join(agent.SummaryHeadings, "\nnone\n") + "\nnone" }

func TestGenerateSummaryThroughMiddleware(t *testing.T) {
	var seen []*schema.AgenticMessage
	fake := &p3SummaryModel{FakeModel: testkit.NewFake(testkit.Step{Text: validSummary()}), inspect: func(_ context.Context, in []*schema.AgenticMessage) { seen = in }}
	h := []agent.AgentMessage{{ID: "u1", Kind: agent.KindUser, Status: agent.StatusComplete, Standard: schema.UserAgenticMessage("build the thing")}}
	got, err := GenerateSummary(t.Context(), fake, "", h, 1024)
	if err != nil || got.Text != validSummary() || got.TemplateVersion != SummaryTemplateVersion || fake.Calls() != 1 {
		t.Fatalf("summary=%+v err=%v calls=%d", got, err, fake.Calls())
	}
	if len(seen) != 2 || seen[0].Role != schema.AgenticRoleTypeSystem || !strings.Contains(seen[1].ContentBlocks[0].UserInputText.Text, "build the thing") {
		t.Fatal("custom model input not used")
	}
	// Incremental: previous summary is input, not re-expanded history.
	if _, err = GenerateSummary(t.Context(), &p3SummaryModel{FakeModel: testkit.NewFake(testkit.Step{Text: validSummary()}), inspect: func(_ context.Context, in []*schema.AgenticMessage) { seen = in }}, "OLD", nil, 1024); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen[1].ContentBlocks[0].UserInputText.Text, "<previous-summary>\nOLD") {
		t.Fatal("previous summary not supplied")
	}
}

func TestGenerateSummaryRejectsInvalidFailedAndCancelled(t *testing.T) {
	h := []agent.AgentMessage{{ID: "u1", Kind: agent.KindUser, Status: agent.StatusComplete, Standard: schema.UserAgenticMessage("x")}}
	if _, err := GenerateSummary(t.Context(), testkit.NewFake(testkit.Step{Text: "## Goal\nonly one"}), "", h, 10); err == nil {
		t.Fatal("summary missing headings accepted")
	}
	sentinel := errors.New("model down")
	if _, err := GenerateSummary(t.Context(), testkit.NewFake(testkit.Step{Err: sentinel}), "", h, 10); !errors.Is(err, sentinel) {
		t.Fatalf("model failure lost: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	fake := &p3SummaryModel{FakeModel: testkit.NewFake(testkit.Step{Text: validSummary()}), inspect: func(context.Context, []*schema.AgenticMessage) { cancel() }}
	if _, err := GenerateSummary(ctx, fake, "", h, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled candidate accepted: %v", err)
	}
	none := testkit.NewFake()
	if _, err := GenerateSummary(t.Context(), none, "", nil, 10); err == nil || none.Calls() != 0 {
		t.Fatal("empty range called the model")
	}
}

func TestGenerateCompactionSummarizesHThenPrefix(t *testing.T) {
	prefix := strings.Join(PrefixHeadings, "\nnone\n") + "\nnone"
	var inputs []string
	fake := &p3SummaryModel{FakeModel: testkit.NewFake(testkit.Step{Text: validSummary()}, testkit.Step{Text: prefix}), inspect: func(_ context.Context, in []*schema.AgenticMessage) {
		inputs = append(inputs, in[1].ContentBlocks[0].UserInputText.Text)
	}}
	h := []agent.AgentMessage{{ID: "u1", Kind: agent.KindUser, Status: agent.StatusComplete, Standard: schema.UserAgenticMessage("history-text")}}
	p := []agent.AgentMessage{{ID: "u2", Kind: agent.KindUser, Status: agent.StatusComplete, Standard: schema.UserAgenticMessage("request-text")}}
	got, err := GenerateCompaction(t.Context(), fake, "", h, p, 1024)
	if err != nil || fake.Calls() != 2 || !strings.Contains(inputs[0], "history-text") || strings.Contains(inputs[0], "request-text") || !strings.Contains(inputs[1], "request-text") {
		t.Fatalf("calls=%d inputs=%q err=%v", fake.Calls(), inputs, err)
	}
	if !strings.HasPrefix(got.Text, validSummary()) || !strings.HasSuffix(got.Text, prefix) {
		t.Fatalf("combined text=%q", got.Text)
	}
	// A prefix answered with the wrong template rejects the whole candidate.
	bad := testkit.NewFake(testkit.Step{Text: validSummary()}, testkit.Step{Text: validSummary()})
	if _, err := GenerateCompaction(t.Context(), bad, "", h, p, 1024); err == nil || bad.Calls() != 2 {
		t.Fatal("invalid work prefix accepted")
	}
	// Only P with a previous summary: the previous text is carried, one call.
	carry := testkit.NewFake(testkit.Step{Text: prefix})
	got, err = GenerateCompaction(t.Context(), carry, validSummary(), nil, p, 1024)
	if err != nil || carry.Calls() != 1 || !strings.HasPrefix(got.Text, validSummary()) {
		t.Fatalf("carry calls=%d err=%v", carry.Calls(), err)
	}
	if _, err := GenerateBranchSummary(t.Context(), testkit.NewFake(testkit.Step{Text: validSummary()}), h, 1024); err == nil {
		t.Fatal("branch summary accepted the main template")
	}
}
