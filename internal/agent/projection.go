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
			text := msg.Command.Name
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
