package eino

import (
	"context"
	"errors"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/testkit"
)

type factSink struct {
	facts []agent.Fact
	err   error
}

func (s *factSink) CommitFact(_ context.Context, _ agent.ExecutionScope, fact agent.Fact) error {
	s.facts = append(s.facts, fact)
	return s.err
}

func TestStreamEOFWithoutFinishDoesNotAcceptTools(t *testing.T) {
	fake := testkit.NewFake(testkit.Step{
		Text: "partial", NoFinish: true,
		ToolCalls: []schema.FunctionToolCall{{CallID: "p1", Name: "add", Arguments: `{"n":1}`}},
	})
	sink := &factSink{}
	vm := NewValidatedModel(fake, sink, agent.NewBudget(config.DefaultLimits()), agent.ExecutionScope{})
	reader, err := vm.Stream(context.Background(), nil)
	if err == nil {
		if reader != nil {
			reader.Close()
		}
		t.Fatal("EOF without seasprak.finish accepted a tool call")
	}
	if len(sink.facts) == 0 {
		t.Fatal("rejection was not persisted")
	}
}

func TestAcceptsToolCallsWithExplicitFinish(t *testing.T) {
	for _, finish := range []string{"stop", "tool_calls"} {
		t.Run(finish, func(t *testing.T) {
			fake := testkit.NewFake(testkit.Step{
				Finish:    finish,
				ToolCalls: []schema.FunctionToolCall{{CallID: "p1", Name: "add", Arguments: `{"n":1}`}},
			})
			sink := &factSink{}
			vm := NewValidatedModel(fake, sink, agent.NewBudget(config.DefaultLimits()), agent.ExecutionScope{TraceID: "tr"})
			msg, err := vm.Generate(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if !hasToolCall(msg) {
				t.Fatal("accepted message dropped the tool call")
			}
		})
	}
}

func TestRejectsBadRoleDuplicateCallIDAndInvalidJSON(t *testing.T) {
	cases := []testkit.Step{
		{Role: schema.AgenticRoleTypeUser, Text: "nope", Finish: "stop"},
		{Finish: "tool_calls", ToolCalls: []schema.FunctionToolCall{
			{CallID: "same", Name: "add", Arguments: `{}`},
			{CallID: "same", Name: "add", Arguments: `{}`},
		}},
		{Finish: "tool_calls", ToolCalls: []schema.FunctionToolCall{{CallID: "p1", Name: "add", Arguments: `{`}}},
	}
	for i, step := range cases {
		fake := testkit.NewFake(step)
		sink := &factSink{}
		vm := NewValidatedModel(fake, sink, agent.NewBudget(config.DefaultLimits()), agent.ExecutionScope{})
		msg, err := vm.Generate(context.Background(), nil)
		if err == nil {
			t.Fatalf("case %d accepted %#v", i, msg)
		}
		if len(sink.facts) == 0 {
			t.Fatalf("case %d did not persist the rejection", i)
		}
	}
}

func TestFailureRecordPersistErrorIsReturned(t *testing.T) {
	fake := testkit.NewFake(testkit.Step{Err: product.NewError(product.CodeResourceUnavailable, "down")})
	sink := &factSink{err: errors.New("persist down")}
	vm := NewValidatedModel(fake, sink, agent.NewBudget(config.DefaultLimits()), agent.ExecutionScope{})
	_, err := vm.Generate(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "persist down") {
		t.Fatalf("err = %v", err)
	}
}

func TestCanceledContextDoesNotAccept(t *testing.T) {
	fake := testkit.NewFake(testkit.Step{Text: "late", Finish: "stop"})
	sink := &factSink{}
	vm := NewValidatedModel(fake, sink, agent.NewBudget(config.DefaultLimits()), agent.ExecutionScope{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msg, err := vm.Generate(ctx, nil)
	if err == nil || msg != nil {
		t.Fatalf("msg=%v err=%v", msg, err)
	}
	if len(sink.facts) == 0 {
		t.Fatal("canceled attempt was not recorded")
	}
}

func TestDirectGenerateBeginsTurn(t *testing.T) {
	fake := testkit.NewFake(testkit.Step{Text: "ok"})
	budg := agent.NewBudget(config.DefaultLimits())
	vm := NewValidatedModel(fake, &factSink{}, budg, agent.ExecutionScope{})
	if _, err := vm.Generate(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if budg.Snapshot().LogicalModelCalls != 1 || budg.Snapshot().TransportRequests != 1 {
		t.Fatalf("usage %+v", budg.Snapshot())
	}
}
