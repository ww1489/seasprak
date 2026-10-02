package web

import (
	"slices"
	"strconv"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent"
)

// The DTOs below are the only network projection of session state. They are
// built field by field from codeagent.Snapshot; internal types are never
// serialized directly, so new private fields cannot leak by default.

type sessionEntryDTO struct {
	SessionID string `json:"sessionId"`
	Available bool   `json:"available"`
	// Name and Labels are display-only metadata, not session facts.
	Name   string   `json:"name,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

type sessionListDTO struct {
	Sessions []sessionEntryDTO `json:"sessions"`
	Next     string            `json:"next,omitempty"`
}

type traceDTO struct {
	TraceID     string `json:"traceId"`
	State       string `json:"state"`
	Kind        string `json:"kind"`
	TargetAgent string `json:"targetAgent"`
	Settled     bool   `json:"settled"`
	Hold        bool   `json:"hold"`
	CanResume   bool   `json:"canResume"`
}

type inputDTO struct {
	InputID string `json:"inputId"`
	TraceID string `json:"traceId"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
}

type blockDTO struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	CallID string `json:"callId,omitempty"`
	Name   string `json:"name,omitempty"`
}

type messageDTO struct {
	MessageID string     `json:"messageId"`
	Kind      string     `json:"kind"`
	TraceID   string     `json:"traceId,omitempty"`
	Status    string     `json:"status"`
	Blocks    []blockDTO `json:"blocks"`
}

type snapshotDTO struct {
	SessionID      string       `json:"sessionId"`
	Revision       uint64       `json:"revision"`
	Cursor         string       `json:"cursor"`
	DurableSeq     string       `json:"durableSeq"`
	ActiveTraceID  string       `json:"activeTraceId,omitempty"`
	RepairRequired bool         `json:"repairRequired"`
	Traces         []traceDTO   `json:"traces"`
	Inputs         []inputDTO   `json:"inputs"`
	Messages       []messageDTO `json:"messages"`
	// InstanceID scopes runtime-only approval receipts and transient display.
	InstanceID string       `json:"instanceId,omitempty"`
	Transient  transientDTO `json:"transient"`
	// Invocations lists delegated child identities and states only; child
	// results, call lists and counts stay server-side.
	Invocations []invocationDTO `json:"invocations"`
	// PendingReconciliations use the reconcile request field names.
	PendingReconciliations []pendingReconciliationDTO `json:"pendingReconciliations"`
}

type pendingReconciliationDTO struct {
	TraceID            string `json:"traceId"`
	InvocationID       string `json:"invocationId"`
	ToolCallID         string `json:"toolCallId"`
	ObservationID      string `json:"observationId"`
	ObservationVersion uint64 `json:"observationVersion"`
}

type invocationDTO struct {
	InvocationID       string `json:"invocationId"`
	ParentInvocationID string `json:"parentInvocationId"`
	TraceID            string `json:"traceId"`
	TargetAgent        string `json:"targetAgent"`
	State              string `json:"state"`
}

type transientModelDTO struct {
	TraceID  string     `json:"traceId"`
	StreamID string     `json:"streamId"`
	ChunkSeq uint64     `json:"chunkSeq"`
	Blocks   []blockDTO `json:"blocks"`
}

type transientToolDTO struct {
	TraceID    string `json:"traceId"`
	ToolCallID string `json:"toolCallId"`
	Stream     string `json:"stream"`
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated"`
}

type transientDTO struct {
	Models []transientModelDTO `json:"models"`
	Tools  []transientToolDTO  `json:"tools"`
}

func projectTransient(v codeagent.TransientView) transientDTO {
	out := transientDTO{Models: []transientModelDTO{}, Tools: []transientToolDTO{}}
	for _, k := range sortedKeys(v.Models) {
		m := v.Models[k]
		dto := transientModelDTO{TraceID: m.TraceID, StreamID: m.Snapshot.StreamID, ChunkSeq: m.Snapshot.ChunkSeq, Blocks: []blockDTO{}}
		for _, b := range m.Snapshot.Blocks {
			// Reasoning stays private in the transient display as well.
			if b.Type == "text" || b.Type == "tool_call" {
				dto.Blocks = append(dto.Blocks, blockDTO{Type: b.Type, Text: b.Text, CallID: b.CallID, Name: b.Name})
			}
		}
		out.Models = append(out.Models, dto)
	}
	for _, k := range sortedKeys(v.Tools) {
		o := v.Tools[k]
		out.Tools = append(out.Tools, transientToolDTO{TraceID: o.TraceID, ToolCallID: o.ToolCallID, Stream: o.Stream, Text: o.Text, Truncated: o.Truncated})
	}
	return out
}

func projectSnapshot(s codeagent.Snapshot) snapshotDTO {
	out := snapshotDTO{
		SessionID: s.SessionID, Revision: s.Revision, Cursor: EncodeCursor(s.SessionID, s.Cursor),
		DurableSeq: strconv.FormatUint(s.Cursor, 10), ActiveTraceID: s.ActiveTrace, RepairRequired: s.RepairRequired,
		Traces: []traceDTO{}, Inputs: []inputDTO{}, Messages: []messageDTO{}, Transient: projectTransient(s.Transient),
	}
	for _, id := range sortedKeys(s.Traces) {
		tr := s.Traces[id]
		if tr == nil {
			continue
		}
		out.Traces = append(out.Traces, traceDTO{TraceID: tr.ID, State: tr.State, Kind: tr.Kind, TargetAgent: tr.Target.Name, Settled: tr.Settled, Hold: tr.Hold, CanResume: s.Resume[id].CanResume})
	}
	for _, id := range sortedKeys(s.Inputs) {
		in := s.Inputs[id]
		if in == nil {
			continue
		}
		out.Inputs = append(out.Inputs, inputDTO{InputID: in.ID, TraceID: in.TraceID, Kind: in.Kind, State: in.State})
	}
	for _, m := range s.Messages {
		out.Messages = append(out.Messages, projectMessage(m))
	}
	out.Invocations = []invocationDTO{}
	for _, id := range sortedKeys(s.Invocations) {
		inv := s.Invocations[id]
		out.Invocations = append(out.Invocations, invocationDTO{InvocationID: inv.ID, ParentInvocationID: inv.ParentInvocationID, TraceID: inv.TraceID, TargetAgent: inv.Target.Name, State: inv.State})
	}
	out.PendingReconciliations = []pendingReconciliationDTO{}
	for _, p := range s.PendingReconciliations {
		out.PendingReconciliations = append(out.PendingReconciliations, pendingReconciliationDTO{TraceID: p.TraceID, InvocationID: p.InvocationID, ToolCallID: p.CallID, ObservationID: p.ObservationID, ObservationVersion: p.ObservationVersion})
	}
	return out
}

// projectMessage exposes only user/assistant text and tool call identities.
// Reasoning, signatures, provider extensions and media payloads stay private.
func projectMessage(m agent.AgentMessage) messageDTO {
	out := messageDTO{MessageID: m.ID, Kind: string(m.Kind), TraceID: m.Scope.TraceID, Status: string(m.Status), Blocks: []blockDTO{}}
	msg := m.Standard
	if msg == nil && m.Custom != nil && m.Custom.Display {
		msg = m.Custom.Content
	}
	if m.Summary != nil {
		out.Blocks = append(out.Blocks, blockDTO{Type: "text", Text: m.Summary.Text})
	}
	if msg == nil {
		return out
	}
	for _, b := range msg.ContentBlocks {
		if b == nil {
			continue
		}
		switch {
		case b.UserInputText != nil:
			out.Blocks = append(out.Blocks, blockDTO{Type: "text", Text: b.UserInputText.Text})
		case b.AssistantGenText != nil:
			out.Blocks = append(out.Blocks, blockDTO{Type: "text", Text: b.AssistantGenText.Text})
		case b.FunctionToolCall != nil:
			out.Blocks = append(out.Blocks, blockDTO{Type: "tool_call", CallID: b.FunctionToolCall.CallID, Name: b.FunctionToolCall.Name})
		case b.FunctionToolResult != nil:
			out.Blocks = append(out.Blocks, blockDTO{Type: "tool_result", CallID: b.FunctionToolResult.CallID, Name: b.FunctionToolResult.Name, Text: toolText(b.FunctionToolResult)})
		}
	}
	return out
}

func toolText(r *schema.FunctionToolResult) string {
	text := ""
	for _, c := range r.Content {
		if c != nil && c.Text != nil {
			text += c.Text.Text
		}
	}
	return text
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
