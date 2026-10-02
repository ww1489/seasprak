// Package workflowagent owns one persistent, independently controlled graph run.
package workflowagent

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/storage"
)

type WorkflowOptions struct {
	Workspace             string
	StateRoot             string
	Store                 storage.Store
	RunID                 string
	Definition            WorkflowDefinition
	Models                map[string]model.AgenticModel
	Tools                 []tools.Definition
	Subflows              map[string]WorkflowDefinition
	Policy                *agent.ResolvedPolicy
	Operations            tools.Operations
	Principal             string
	Limits                config.Limits
	ReadOnly              bool
	GenerationFingerprint string
}

type WorkflowInputCommand struct {
	Input            json.RawMessage `json:"input"`
	IdempotencyKey   string          `json:"idempotencyKey,omitempty"`
	ExpectedRevision *uint64         `json:"expectedRevision,omitempty"`
	Principal        string          `json:"-"`
}
type WorkflowInputReceipt struct {
	RunID             string `json:"runId"`
	InputID           string `json:"inputId"`
	DefinitionVersion string `json:"definitionVersion"`
	BindingVersion    string `json:"bindingVersion"`
	State             string `json:"state"`
	AcceptedCommit    uint64 `json:"acceptedCommit"`
}
type WorkflowControlCommand struct {
	IdempotencyKey   string  `json:"idempotencyKey,omitempty"`
	ExpectedRevision *uint64 `json:"expectedRevision,omitempty"`
	Reason           string  `json:"reason,omitempty"`
	Principal        string  `json:"-"`
}
type WorkflowInteractionResponse struct {
	InteractionID    string `json:"interactionId"`
	Decision         string `json:"decision"`
	ExpectedRevision uint64 `json:"expectedRevision"`
	IdempotencyKey   string `json:"idempotencyKey,omitempty"`
	Principal        string `json:"-"`
}
type WorkflowOperationReceipt struct {
	OperationID    string `json:"operationId"`
	State          string `json:"state"`
	Target         string `json:"target"`
	AcceptedCommit uint64 `json:"acceptedCommit"`
	ReceiptScope   string `json:"receiptScope"`
	InstanceID     string `json:"instanceId,omitempty"`
}
type WorkflowOperation struct {
	Receipt   WorkflowOperationReceipt `json:"receipt"`
	Kind      string                   `json:"kind"`
	State     string                   `json:"state"`
	Principal string                   `json:"-"`
	Key       string                   `json:"-"`
	Digest    string                   `json:"-"`
}
type NodeRun struct {
	ID                string      `json:"nodeExecutionId"`
	NodeID            string      `json:"nodeId"`
	Path              string      `json:"path"`
	InvocationID      string      `json:"invocationId"`
	ChildInvocationID string      `json:"childInvocationId,omitempty"`
	DefinitionHash    string      `json:"definitionHash"`
	Kind              string      `json:"kind"`
	State             string      `json:"state"`
	Ordinal           int         `json:"ordinal"`
	ToolCallID        string      `json:"toolCallId,omitempty"`
	Result            string      `json:"-"`
	ErrorCode         string      `json:"errorCode,omitempty"`
	Usage             agent.Usage `json:"-"`
}
type WorkflowInteraction struct {
	ID              string    `json:"interactionId"`
	NodeExecutionID string    `json:"nodeExecutionId"`
	ToolCallID      string    `json:"toolCallId"`
	Question        string    `json:"question"`
	Options         []string  `json:"options"`
	State           string    `json:"state"`
	ExpiresAt       time.Time `json:"expiresAt"`
	InstanceID      string    `json:"instanceId"`
}
type WorkflowModelAttemptView struct {
	ID              string `json:"attemptId"`
	NodeExecutionID string `json:"nodeExecutionId"`
	ModelCallID     string `json:"modelCallId"`
	Status          string `json:"status"`
	FailureCode     string `json:"failureCode,omitempty"`
}

type WorkflowObservationView struct {
	ToolCallID      string `json:"toolCallId"`
	NodeExecutionID string `json:"nodeExecutionId"`
	Status          string `json:"status"`
	SideEffect      string `json:"sideEffect"`
	Executed        bool   `json:"executed"`
	Process         bool   `json:"process"`
	Terminated      bool   `json:"terminated"`
	ExitCode        int    `json:"exitCode"`
}

type WorkflowSnapshot struct {
	ModelAttempts        map[string]WorkflowModelAttemptView `json:"modelAttempts"`
	Observations         map[string]WorkflowObservationView  `json:"observations"`
	RunID                string                              `json:"runId"`
	DefinitionName       string                              `json:"definitionName"`
	DefinitionVersion    string                              `json:"definitionVersion"`
	BindingVersion       string                              `json:"bindingVersion"`
	State                string                              `json:"state"`
	Revision             uint64                              `json:"revision"`
	DurableSeq           uint64                              `json:"-"`
	Cursor               string                              `json:"cursor"`
	EarliestReplayCursor string                              `json:"earliestReplayCursor"`
	InstanceID           string                              `json:"instanceId"`
	Workspace            string                              `json:"-"`
	ExecutionStopped     bool                                `json:"executionStopped"`
	WorkflowNodes        map[string]NodeRun                  `json:"workflowNodes"`
	Interactions         map[string]WorkflowInteraction      `json:"interactions"`
	Operations           map[string]WorkflowOperation        `json:"operations"`
	Usage                agent.Usage                         `json:"-"`
	Result               json.RawMessage                     `json:"-"`
	ErrorCode            string                              `json:"errorCode,omitempty"`
	FailedNode           string                              `json:"failedNode,omitempty"`
	CanResume            bool                                `json:"canResume"`
	ResumeCode           string                              `json:"resumeCode,omitempty"`
	RepairRequired       bool                                `json:"repairRequired"`
	Transient            WorkflowTransient                   `json:"-"`
}
type WorkflowToolPreview struct {
	agent.ToolOutputDelta
	Truncated bool
	order     uint64
}

type WorkflowTransient struct {
	Models map[string]agent.ModelStreamSnapshot
	Tools  map[string]WorkflowToolPreview
}
type WorkflowSubscribeOptions struct {
	Cursor string
	After  uint64
	Limits config.Limits
}

// All durable mutations use commitLocked. The mutex never encloses calls into
// a ledger, tool, model, resource scheduler or untrusted observer.
type WorkflowAgent struct {
	mu           sync.Mutex
	opts         WorkflowOptions
	store        storage.Store
	state        runState
	compiled     *CompiledWorkflow
	binding      string
	active       *segment
	closing      bool
	closed       bool
	broken       error
	instance     string
	approvals    map[string]*runtimeApproval
	approvalOps  map[string]WorkflowOperation
	subs         map[*WorkflowSubscription]struct{}
	transient    WorkflowTransient
	streamSeq    map[string]uint64
	callStreams  map[string]string
	previewOrder uint64
	scheduler    *tools.ResourceScheduler
	closeDone    chan struct{}
	closeErr     error
	closeCause   error
}

type segment struct {
	scope      agent.ExecutionScope
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	startedAt  time.Time
	gateClosed bool
	subflows   int
	nodes      sync.WaitGroup
	semaphore  chan struct{}
	from       string
}

type modelResult struct {
	Status    string                    `json:"status"`
	Message   *schema.AgenticMessage    `json:"message,omitempty"`
	AttemptID string                    `json:"attemptId"`
	Details   agent.ModelAttemptDetails `json:"details"`
}

type runtimeApproval struct {
	view               WorkflowInteraction
	frozen             agent.FrozenExecution
	decision           string
	requestedExecution string
	stoppedExecution   string
	claimedExecution   string
}
