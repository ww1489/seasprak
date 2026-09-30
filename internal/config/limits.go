package config

import "time"

// Web limits protect the local HTTP transport, not Agent execution budgets.
const (
	WebJSONBytes         = 4 << 20
	WebHeaderBytes       = 32 << 10
	WebReadHeaderTimeout = 5 * time.Second
	WebIdleTimeout       = time.Minute
	WebShutdownTimeout   = 5 * time.Second
)

// Attachment limits apply to the raw request body independently of the JSON
// limit; no request encoding can raise them. Names are display data only.
const (
	WebAttachmentBytes       = 8 << 20
	WebAttachmentsPerSession = 64
	WebAttachmentNameBytes   = 255
	// InputAttachments bounds the attachments referenced by one input;
	// InputAttachmentTextBytes bounds each text attachment given to a model.
	InputAttachments         = 8
	InputAttachmentTextBytes = 256 << 10
)

// Session display metadata limits. Metadata is not a session fact.
const (
	SessionNameBytes        = 256
	SessionLabels           = 16
	SessionLabelBytes       = 64
	SessionMetadataKeysKept = 64
)

// ToolOutputChunkBytes is the maximum UTF-8 text size of one temporary output event.
const ToolOutputChunkBytes = 50 << 10

// Temporary display aggregation held in the session mailbox for snapshots.
// Per-stream previews keep the newest bytes and mark truncation; the session
// total drops the oldest streams first. None of this is durable.
const (
	TransientToolPreviewBytes = 64 << 10
	TransientSessionBytes     = 1 << 20
)

// SSE transport: heartbeat interval and per-write deadline. There is no total
// stream write timeout.
const (
	WebSSEHeartbeat     = 15 * time.Second
	WebSSEWriteDeadline = 10 * time.Second
)

// GeminiFunctionArgumentsBytes bounds gateway JSON argument fragments per call.
const GeminiFunctionArgumentsBytes = 1 << 20

// GeminiCacheRegistryEntries bounds cached and in-flight resources per registered factory.
const GeminiCacheRegistryEntries = 128

// Delegation limits: concurrent child invocations per trace, nesting layers,
// and the bounded child text returned as the single parent tool result.
const (
	SubagentConcurrency = 4
	SubagentDepth       = 4
	DelegateResultBytes = 32 << 10
)

// ApprovalValidity is checked both at the decision and atomic claim boundaries.
const ApprovalValidity = 24 * time.Hour

// Limits holds the engineering protection defaults from the delivery plan.
type Limits struct {
	LogicalModelRequests   int
	TraceLogicalModelCalls int
	TraceTransportRequests int
	TraceToolCalls         int
	TraceCompactions       int
	OverflowRecoveries     int
	ActivityBudget         time.Duration
	ToolTimeout            time.Duration
	HookTimeout            time.Duration
	ReadConcurrency        int
	SubscriptionEvents     int
	SubscriptionBytes      int
	MaxCommitLineBytes     int
}

func DefaultLimits() Limits {
	return Limits{
		LogicalModelRequests:   3,
		TraceLogicalModelCalls: 128,
		TraceTransportRequests: 256,
		TraceToolCalls:         512,
		TraceCompactions:       8,
		OverflowRecoveries:     1,
		ActivityBudget:         30 * time.Minute,
		ToolTimeout:            120 * time.Second,
		HookTimeout:            10 * time.Second,
		ReadConcurrency:        4,
		SubscriptionEvents:     256,
		SubscriptionBytes:      2 << 20,
		MaxCommitLineBytes:     1 << 20,
	}
}

func (l Limits) WithDefaults() Limits {
	d := DefaultLimits()
	if l.LogicalModelRequests == 0 {
		l.LogicalModelRequests = d.LogicalModelRequests
	}
	if l.TraceLogicalModelCalls == 0 {
		l.TraceLogicalModelCalls = d.TraceLogicalModelCalls
	}
	if l.TraceTransportRequests == 0 {
		l.TraceTransportRequests = d.TraceTransportRequests
	}
	if l.TraceToolCalls == 0 {
		l.TraceToolCalls = d.TraceToolCalls
	}
	if l.TraceCompactions == 0 {
		l.TraceCompactions = d.TraceCompactions
	}
	if l.OverflowRecoveries == 0 {
		l.OverflowRecoveries = d.OverflowRecoveries
	}
	if l.ActivityBudget == 0 {
		l.ActivityBudget = d.ActivityBudget
	}
	if l.ToolTimeout == 0 {
		l.ToolTimeout = d.ToolTimeout
	}
	if l.HookTimeout == 0 {
		l.HookTimeout = d.HookTimeout
	}
	if l.ReadConcurrency == 0 {
		l.ReadConcurrency = d.ReadConcurrency
	}
	if l.SubscriptionEvents == 0 {
		l.SubscriptionEvents = d.SubscriptionEvents
	}
	if l.SubscriptionBytes == 0 {
		l.SubscriptionBytes = d.SubscriptionBytes
	}
	if l.MaxCommitLineBytes == 0 {
		l.MaxCommitLineBytes = d.MaxCommitLineBytes
	}
	return l
}
