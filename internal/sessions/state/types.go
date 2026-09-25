package state

import (
	"encoding/json"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
)

type TraceState struct {
	ID           string            `json:"id"`
	State        string            `json:"state"`
	Kind         string            `json:"kind"`
	Target       agent.TargetAgent `json:"target"`
	Generation   string            `json:"generation"`
	Settled      bool              `json:"settled"`
	Hold         bool              `json:"hold"`
	Started      bool              `json:"started"`
	InvocationID string            `json:"invocationId,omitempty"`
	Limits       config.Limits     `json:"limits"`
	Usage        agent.Usage       `json:"usage"`
	HoldOnStop   []string          `json:"holdOnStop,omitempty"`
	Error        string            `json:"error,omitempty"`

	// ExecutionStopped is durable proof of an exited execution, not known effects.
	ExecutionStopped bool `json:"executionStopped,omitempty"`
}
type InputState struct {
	ID        string            `json:"id"`
	TraceID   string            `json:"traceId"`
	Kind      string            `json:"kind"`
	State     string            `json:"state"`
	Content   json.RawMessage   `json:"content"`
	Principal string            `json:"principal"`
	Target    agent.TargetAgent `json:"target"`
	CommitSeq uint64            `json:"commitSeq"`
}
type View struct {
	LastSeq        uint64
	Cursor         uint64
	BranchID       string
	LeafID         string
	Traces         map[string]*TraceState
	Inputs         map[string]*InputState
	Order          []string
	Steering       []string
	Follow         []string
	Independent    []string
	Idem           map[string]idemRecord
	Budget         agent.Usage // compatibility: aggregate diagnostic; limits are per Trace
	Generation     string
	Events         []agent.Event
	ActiveTrace    string
	Messages       []agent.AgentMessage
	Turns          map[string]agent.TurnRecord
	Calls          map[string]agent.ToolRecord
	RepairRequired bool
}
type idemRecord struct {
	Digest  string
	Receipt agent.InputReceipt
}
