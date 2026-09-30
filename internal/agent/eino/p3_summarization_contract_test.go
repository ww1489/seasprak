package eino

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/testkit"
)

// These are v0.9.21 framework contracts, not product compaction or commit tests.
type p3SummaryModel struct {
	*testkit.FakeModel
	inspect func(context.Context, []*schema.AgenticMessage)
}

func (m *p3SummaryModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	if m.inspect != nil {
		m.inspect(ctx, in)
	}
	return m.FakeModel.Generate(ctx, in, opts...)
}

func p3SummaryMiddleware(t *testing.T, cfg *summarization.TypedConfig[*schema.AgenticMessage]) *summarization.TypedMiddleware[*schema.AgenticMessage] {
	t.Helper()
	mw, err := summarization.NewTyped(t.Context(), cfg)
	if err != nil {
		t.Fatal("construct summarization middleware failed")
	}
	typed, ok := mw.(*summarization.TypedMiddleware[*schema.AgenticMessage])
	if !ok {
		t.Fatal("Agentic middleware type mismatch")
	}
	return typed
}

func p3SummaryFixture(t *testing.T) (original, candidate *adk.TypedChatModelAgentState[*schema.AgenticMessage]) {
	t.Helper()
	original = &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{
		schema.SystemAgenticMessage("synthetic policy"), schema.UserAgenticMessage("synthetic history"), schema.UserAgenticMessage("synthetic recent"),
	}, ToolInfos: []*schema.ToolInfo{testkit.ToolInfo("probe", "synthetic tool")}}
	// Fixture-only deep copy. Summarize itself only shallow-copies the state.
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal("encode synthetic fixture failed")
	}
	candidate = new(adk.TypedChatModelAgentState[*schema.AgenticMessage])
	if json.Unmarshal(raw, candidate) != nil {
		t.Fatal("copy synthetic fixture failed")
	}
	return original, candidate
}

func TestP3SummarizationAgenticCandidateHooks(t *testing.T) {
	original, candidate := p3SummaryFixture(t)
	before, _ := json.Marshal(original)
	var order []string
	fake := &p3SummaryModel{FakeModel: testkit.NewFake(testkit.Step{Text: "synthetic summary"})}
	fake.inspect = func(_ context.Context, in []*schema.AgenticMessage) {
		order = append(order, "model")
		if len(in) != 1 || in[0] != candidate.Messages[1] {
			t.Error("custom input did not reach model")
		}
	}
	mw := p3SummaryMiddleware(t, &summarization.TypedConfig[*schema.AgenticMessage]{
		Model: fake,
		TokenCounter: func(context.Context, *summarization.TypedTokenCounterInput[*schema.AgenticMessage]) (int, error) {
			t.Error("explicit Summarize must not count tokens")
			return 0, nil
		},
		GenModelInput: func(_ context.Context, sys, user *schema.AgenticMessage, msgs []*schema.AgenticMessage) ([]*schema.AgenticMessage, error) {
			order = append(order, "input")
			if sys.Role != schema.AgenticRoleTypeSystem || user.Role != schema.AgenticRoleTypeUser || len(msgs) != 3 {
				t.Error("unexpected typed instructions or source")
			}
			// A hook may mutate its input; only the caller-owned candidate may be affected.
			msgs[1].Extra = map[string]any{"probe": true}
			return msgs[1:2], nil
		},
		Finalize: func(_ context.Context, msgs []*schema.AgenticMessage, summary *schema.AgenticMessage) ([]*schema.AgenticMessage, error) {
			order = append(order, "finalize")
			if summary.Role != schema.AgenticRoleTypeAssistant || len(summary.ContentBlocks) != 1 || summary.ContentBlocks[0].AssistantGenText.Text != "synthetic summary" {
				t.Error("raw summary altered before custom finalizer")
			}
			return []*schema.AgenticMessage{summary, msgs[2]}, nil
		},
		Callback: func(_ context.Context, before, after adk.TypedChatModelAgentState[*schema.AgenticMessage]) error {
			order = append(order, "callback")
			if len(before.Messages) != 3 || len(after.Messages) != 2 || after.Messages[1] != candidate.Messages[2] {
				t.Error("callback candidate mismatch")
			}
			return nil // Notification only; no persistence is performed by this probe.
		},
	})
	out, err := mw.Summarize(t.Context(), candidate)
	if err != nil || len(out) != 2 || fake.Calls() != 1 {
		t.Fatal("candidate generation contract failed")
	}
	if !reflect.DeepEqual(order, []string{"input", "model", "finalize", "callback"}) {
		t.Fatal("hook order or invocation count changed")
	}
	after, _ := json.Marshal(original)
	if string(before) != string(after) || len(candidate.Messages) != 3 || candidate.Messages[1].Extra["probe"] != true {
		t.Fatal("candidate ownership contract failed")
	}
	// Custom Finalize bypasses the default user-role summary and transcript appendix.
	if out[0].Role != schema.AgenticRoleTypeAssistant {
		t.Fatal("default post-processing unexpectedly applied")
	}
}

func TestP3SummarizationTokenCounterBoundary(t *testing.T) {
	sentinel := errors.New("synthetic counter failure")
	for _, tc := range []struct {
		name   string
		tokens int
		err    error
		calls  int
	}{
		{"equal", 10, nil, 0}, {"above", 11, nil, 1}, {"counter_error", 0, sentinel, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, state := p3SummaryFixture(t)
			counts := 0
			fake := testkit.NewFake(testkit.Step{Text: "synthetic summary"})
			mw := p3SummaryMiddleware(t, &summarization.TypedConfig[*schema.AgenticMessage]{Model: fake, Trigger: &summarization.TriggerCondition{ContextTokens: 10},
				TokenCounter: func(_ context.Context, in *summarization.TypedTokenCounterInput[*schema.AgenticMessage]) (int, error) {
					counts++
					if len(in.Messages) != 3 || len(in.Tools) != 1 || in.Tools[0] != state.ToolInfos[0] {
						t.Error("counter input mismatch")
					}
					return tc.tokens, tc.err
				},
				Finalize: func(_ context.Context, _ []*schema.AgenticMessage, summary *schema.AgenticMessage) ([]*schema.AgenticMessage, error) {
					return []*schema.AgenticMessage{summary}, nil
				},
			})
			_, out, err := mw.BeforeModelRewriteState(t.Context(), state, nil)
			if !errors.Is(err, tc.err) || counts != 1 || fake.Calls() != tc.calls {
				t.Fatal("counter boundary or invocation count mismatch")
			}
			if tc.err != nil {
				if out != nil {
					t.Fatal("counter error produced state")
				}
				return
			}
			if tc.calls == 0 && out != state {
				t.Fatal("non-triggered state replaced")
			}
			if tc.calls == 1 && (out == state || len(out.Messages) != 1 || len(state.Messages) != 3) {
				t.Fatal("triggered state ownership mismatch")
			}
		})
	}
}

func TestP3SummarizationFailureProducesNoCandidate(t *testing.T) {
	sentinel := errors.New("synthetic summary failure")
	for _, stage := range []string{"input", "model", "finalize", "callback", "model_cancel", "finalize_cancel"} {
		t.Run(stage, func(t *testing.T) {
			original, state := p3SummaryFixture(t)
			before, _ := json.Marshal(original)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			step := testkit.Step{Text: "synthetic summary"}
			if stage == "model" {
				step.Err = sentinel
			}
			if stage == "model_cancel" {
				step.Gate = make(chan struct{})
			}
			fake := &p3SummaryModel{FakeModel: testkit.NewFake(step)}
			fake.inspect = func(context.Context, []*schema.AgenticMessage) {
				if stage == "model_cancel" || stage == "finalize_cancel" {
					cancel()
				}
			}
			input, finalize, callback := 0, 0, 0
			mw := p3SummaryMiddleware(t, &summarization.TypedConfig[*schema.AgenticMessage]{Model: fake,
				GenModelInput: func(_ context.Context, _, _ *schema.AgenticMessage, msgs []*schema.AgenticMessage) ([]*schema.AgenticMessage, error) {
					input++
					if stage == "input" {
						return nil, sentinel
					}
					return msgs, nil
				},
				Finalize: func(ctx context.Context, _ []*schema.AgenticMessage, summary *schema.AgenticMessage) ([]*schema.AgenticMessage, error) {
					finalize++
					// Framework does not enforce cancellation after a successful Generate.
					// A candidate validator must reject cancellation, including non-cooperative models.
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					if stage == "finalize" {
						return nil, sentinel
					}
					return []*schema.AgenticMessage{summary}, nil
				},
				Callback: func(context.Context, adk.TypedChatModelAgentState[*schema.AgenticMessage], adk.TypedChatModelAgentState[*schema.AgenticMessage]) error {
					callback++
					if stage == "callback" {
						return sentinel
					}
					return nil
				},
			})
			out, err := mw.Summarize(ctx, state)
			want := sentinel
			if stage == "model_cancel" || stage == "finalize_cancel" {
				want = context.Canceled
			}
			if !errors.Is(err, want) || out != nil {
				t.Fatal("failure produced candidate or lost error identity")
			}
			wantModel, wantFinalize, wantCallback := 1, 0, 0
			if stage == "input" {
				wantModel = 0
			}
			if stage == "finalize" || stage == "finalize_cancel" || stage == "callback" {
				wantFinalize = 1
			}
			if stage == "callback" {
				wantCallback = 1
			}
			if input != 1 || fake.Calls() != wantModel || finalize != wantFinalize || callback != wantCallback {
				t.Fatal("failure hook invocation counts mismatch")
			}
			after, _ := json.Marshal(original)
			if string(before) != string(after) {
				t.Fatal("original input mutated")
			}
		})
	}
}

func TestP3SummarizationFrameworkRequiresCancellationGuard(t *testing.T) {
	_, state := p3SummaryFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fake := &p3SummaryModel{FakeModel: testkit.NewFake(testkit.Step{Text: "synthetic summary"}), inspect: func(context.Context, []*schema.AgenticMessage) { cancel() }}
	finalizations := 0
	mw := p3SummaryMiddleware(t, &summarization.TypedConfig[*schema.AgenticMessage]{Model: fake,
		Finalize: func(context.Context, []*schema.AgenticMessage, *schema.AgenticMessage) ([]*schema.AgenticMessage, error) {
			finalizations++
			return []*schema.AgenticMessage{schema.UserAgenticMessage("unvalidated synthetic candidate")}, nil
		},
	})
	out, err := mw.Summarize(ctx, state)
	// Characterize a framework gap, not a valid product candidate: v0.9.21
	// trusts the model/finalizer and does not itself reject a cancelled context.
	if ctx.Err() != context.Canceled || err != nil || len(out) != 1 || fake.Calls() != 1 || finalizations != 1 {
		t.Fatal("framework cancellation responsibility changed")
	}
}
