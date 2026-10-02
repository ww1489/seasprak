package eino

import (
	"context"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
)

// ChildContext gives a delegated run its own mutable scope and clears the
// parent's turn marker, so child boundaries never rewrite the parent scope or
// skip their own logical-call reservation.
func ChildContext(ctx context.Context, scope agent.ExecutionScope) context.Context {
	return WithExecutionScope(context.WithValue(ctx, turnKey{}, false), scope)
}

// RunDelegated runs a child agent once with the explicit task as its only user
// message and returns the text of its last assistant message. It does not
// share parent history, checkpoints or the parent TurnLoop.
func RunDelegated(ctx context.Context, ag adk.TypedAgent[*schema.AgenticMessage], task string, enableStreaming bool) (string, error) {
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag, EnableStreaming: enableStreaming})
	iter := runner.Run(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage(task)})
	var final string
	var runErr error
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			if runErr == nil {
				runErr = ev.Err
			}
			continue
		}
		if ev.Output == nil || ev.Output.MessageOutput == nil {
			continue
		}
		msg, err := ev.Output.MessageOutput.GetMessage()
		if err != nil {
			if runErr == nil {
				runErr = err
			}
			continue
		}
		if msg != nil && msg.Role == schema.AgenticRoleTypeAssistant {
			final = assistantText(msg)
		}
	}
	if runErr == nil {
		runErr = ctx.Err()
	}
	return final, runErr
}

func assistantText(msg *schema.AgenticMessage) string {
	var b strings.Builder
	for _, block := range msg.ContentBlocks {
		if block != nil && block.AssistantGenText != nil {
			b.WriteString(block.AssistantGenText.Text)
		}
	}
	return b.String()
}
