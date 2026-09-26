package eino

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

type attemptUsageKey struct{}
type attemptUsage struct {
	mu       sync.Mutex
	identity agent.ModelAttemptIdentity
	items    map[uint64]agent.ModelRequestUsage
}

func (u *attemptUsage) ObserveUsage(_ context.Context, request llm.TransportRequest, snapshot llm.UsageSnapshot) {
	if request.AttemptID != u.identity.ID || request.ModelCallID != u.identity.ModelCallID {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.items[request.TransportAttempt] = agent.ModelRequestUsage{Request: request, Snapshot: snapshot}
}
func attemptDetails(ctx context.Context, cause error) agent.ModelAttemptDetails {
	d := agent.ModelAttemptDetails{}
	if u, ok := ctx.Value(attemptUsageKey{}).(*attemptUsage); ok {
		u.mu.Lock()
		for _, item := range u.items {
			d.Usage = append(d.Usage, item)
		}
		u.mu.Unlock()
		sort.Slice(d.Usage, func(i, j int) bool { return d.Usage[i].Request.TransportAttempt < d.Usage[j].Request.TransportAttempt })
	}
	if cause == nil {
		return d
	}
	d.FailureCode = product.CodeResourceUnavailable
	d.FailureReason = "model_error"
	switch {
	case errors.Is(cause, context.Canceled):
		d.FailureReason = "cancelled"
	case errors.Is(cause, context.DeadlineExceeded):
		d.FailureReason = "deadline_exceeded"
	default:
		if info, ok := llm.ModelFailure(cause); ok {
			d.FailureReason = info.Kind
		}
		var pe *product.Error
		if errors.As(cause, &pe) && pe != nil {
			switch pe.Code {
			case product.CodeStorageUnavailable, product.CodeBudgetExhausted, product.CodePermissionDenied, product.CodeUnauthenticated, product.CodeInvalidArgument, product.CodeUnsupportedCapability, product.CodeStateConflict, product.CodeInternal, product.CodeResourceUnavailable:
				d.FailureCode = pe.Code
			}
		}
	}
	return d
}

// A failed candidate will never be replayed. Keep only visible text and the
// finite normalized finish; signatures, provider metadata and media references
// are neither needed for replay nor safe diagnostic payloads.
func diagnosticPartial(msg *schema.AgenticMessage) *schema.AgenticMessage {
	if msg == nil || msg.Role != schema.AgenticRoleTypeAssistant {
		return nil
	}
	out := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant}
	switch finish := finishReason(msg); finish {
	case "stop", "tool_calls", "length", "refusal":
		out.Extra = map[string]any{"seasprak.finish": finish}
	}
	for _, b := range msg.ContentBlocks {
		if b == nil {
			continue
		}
		if b.Type == schema.ContentBlockTypeAssistantGenText && b.AssistantGenText != nil {
			out.ContentBlocks = append(out.ContentBlocks, schema.NewContentBlock(&schema.AssistantGenText{Text: b.AssistantGenText.Text}))
		}
		if b.Type == schema.ContentBlockTypeReasoning && b.Reasoning != nil {
			out.ContentBlocks = append(out.ContentBlocks, schema.NewContentBlock(&schema.Reasoning{Text: b.Reasoning.Text}))
		}
	}
	return out
}
