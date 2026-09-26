package agent

import "context"

// ApprovalWait carries only the durable product interaction identity. The Eino
// adapter turns it into a framework interrupt; it is never a failed tool result.
type ApprovalWait struct {
	InteractionID string
}

func (*ApprovalWait) Error() string { return "tool execution is waiting for approval" }

type ApprovalRequester interface {
	RequestToolApproval(context.Context, ExecutionScope, FrozenExecution) (*ApprovalWait, error)
}
