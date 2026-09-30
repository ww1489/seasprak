package agent

import (
	"github.com/cloudwego/eino/schema"

	product "github.com/ww1489/seasprak/internal/errors"
)

// TransformContext copies messages and drops incomplete failures.
// It does not delete an accepted tool-call group or change authorization.
func TransformContext(in []AgentMessage) ([]AgentMessage, error) {
	out := make([]AgentMessage, 0, len(in))
	for _, msg := range in {
		if err := msg.Validate(); err != nil {
			return nil, err
		}
		if msg.Status == StatusIncomplete {
			continue
		}
		if msg.Kind == KindOpaque && msg.Opaque.RequiredForModel {
			return nil, product.NewError(product.CodeUnsupportedCapability, "opaque message has no projection rule")
		}
		out = append(out, msg)
	}
	return out, nil
}

// ProjectHistory applies the newest compaction on the selected path: the
// result is that summary followed by the path from its FirstKeptID. Kept
// entries precede the summary on the path, so they are re-emitted after it.
func ProjectHistory(path []AgentMessage) []AgentMessage {
	for i := len(path) - 1; i >= 0; i-- {
		s := path[i]
		if s.Kind != KindCompactionSummary || s.Summary == nil || s.Summary.FirstKeptID == "" {
			continue
		}
		kept := -1
		for j := 0; j < i; j++ {
			if path[j].ID == s.Summary.FirstKeptID {
				kept = j
				break
			}
		}
		if kept < 0 {
			continue
		}
		out := make([]AgentMessage, 0, 1+(i-kept)+(len(path)-i-1))
		out = append(out, s)
		out = append(out, path[kept:i]...)
		return append(out, path[i+1:]...)
	}
	return path
}

// ConvertToLLM projects already selected messages. It does not read the network or history.
func ConvertToLLM(in []AgentMessage) ([]*schema.AgenticMessage, error) {
	selected, err := TransformContext(in)
	if err != nil {
		return nil, err
	}
	out := make([]*schema.AgenticMessage, 0, len(selected))
	for _, msg := range selected {
		switch msg.Kind {
		case KindUser, KindAssistant, KindToolResult:
			out = append(out, msg.Standard)
		case KindCustom:
			if msg.Custom.Content == nil {
				return nil, product.NewError(product.CodeInvalidArgument, "custom content is required")
			}
			copied := *msg.Custom.Content
			copied.Role = schema.AgenticRoleTypeUser
			out = append(out, &copied)
		case KindCommand:
			if msg.Command.ExcludeFromContext {
				continue
			}
			text := msg.Command.Name
			if text == "shell" {
				text, err = hostShellText(*msg.Command)
				if err != nil {
					return nil, err
				}
			}
			out = append(out, schema.UserAgenticMessage(text))
		case KindCompactionSummary, KindBranchSummary:
			out = append(out, schema.UserAgenticMessage(msg.Summary.Text))
		case KindOpaque:
			continue
		default:
			return nil, product.Errorf(product.CodeInvalidArgument, "cannot project %s", msg.Kind)
		}
	}
	return out, nil
}
