package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

// A2UI presentation protocol of 13-p3-web-contract §7.1. The envelope and the
// Text/Column/Card/Row subset follow cloudwego/eino-examples
// quickstart/chatwitheino/a2ui/types.go at commit
// a6dbd95ab51fe9896a2bafa2e5a468e3bed01161 (Copyright 2026 CloudWeGo Authors,
// Apache-2.0). Modifications: interruptRequest is removed, product components
// ChatMessage/ToolCall/Task/Approval are added, and all types are unexported
// network DTOs built field by field from public session facts.

// a2uiMessage carries exactly one populated field.
type a2uiMessage struct {
	BeginRendering  *a2uiBeginRendering  `json:"beginRendering,omitempty"`
	SurfaceUpdate   *a2uiSurfaceUpdate   `json:"surfaceUpdate,omitempty"`
	DataModelUpdate *a2uiDataModelUpdate `json:"dataModelUpdate,omitempty"`
	DeleteSurface   *a2uiDeleteSurface   `json:"deleteSurface,omitempty"`
}

type a2uiBeginRendering struct {
	SurfaceID string `json:"surfaceId"`
	Root      string `json:"root"`
}

type a2uiSurfaceUpdate struct {
	SurfaceID  string          `json:"surfaceId"`
	Components []a2uiComponent `json:"components"`
}

type a2uiDataModelUpdate struct {
	SurfaceID string        `json:"surfaceId"`
	Contents  []a2uiContent `json:"contents"`
}

type a2uiDeleteSurface struct {
	SurfaceID string `json:"surfaceId"`
}

type a2uiContent struct {
	Key         string `json:"key"`
	ValueString string `json:"valueString"`
}

type a2uiComponent struct {
	ID        string             `json:"id"`
	Component a2uiComponentValue `json:"component"`
}

// a2uiComponentValue carries exactly one populated component.
type a2uiComponentValue struct {
	Text        *a2uiText        `json:"Text,omitempty"`
	Column      *a2uiChildren    `json:"Column,omitempty"`
	Card        *a2uiChildren    `json:"Card,omitempty"`
	Row         *a2uiChildren    `json:"Row,omitempty"`
	ChatMessage *a2uiChatMessage `json:"ChatMessage,omitempty"`
	ToolCall    *a2uiToolCall    `json:"ToolCall,omitempty"`
	Task        *a2uiTask        `json:"Task,omitempty"`
	Approval    *a2uiApproval    `json:"Approval,omitempty"`
	// Invocation and WorkflowNode are progress components (§7.1). They carry
	// identity, name and state only; results and errors stay private.
	Invocation   *a2uiInvocation   `json:"Invocation,omitempty"`
	WorkflowNode *a2uiWorkflowNode `json:"WorkflowNode,omitempty"`
}

type a2uiInvocation struct {
	InvocationID string `json:"invocationId"`
	ParentCallID string `json:"parentCallId"`
	Agent        string `json:"agent"`
	State        string `json:"state"`
}

type a2uiWorkflowNode struct {
	NodeExecutionID string `json:"nodeExecutionId"`
	TraceID         string `json:"traceId"`
	NodeID          string `json:"nodeId"`
	Kind            string `json:"kind"`
	State           string `json:"state"`
}

type a2uiText struct {
	Value     string `json:"value,omitempty"`
	DataKey   string `json:"dataKey,omitempty"`
	UsageHint string `json:"usageHint,omitempty"`
}

type a2uiChildren struct {
	Children []string `json:"children"`
}

type a2uiChatMessage struct {
	MessageID string `json:"messageId"`
	Role      string `json:"role"`   // user | assistant | tool | summary
	Status    string `json:"status"` // final | streaming
	DataKey   string `json:"dataKey"`
}

type a2uiToolCall struct {
	CallID string `json:"callId"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type a2uiTask struct {
	TraceID     string `json:"traceId"`
	State       string `json:"state"`
	TargetAgent string `json:"targetAgent"`
	Settled     bool   `json:"settled"`
}

type a2uiApproval struct {
	InteractionID string   `json:"interactionId"`
	TraceID       string   `json:"traceId"`
	Question      string   `json:"question"`
	Options       []string `json:"options"`
	InstanceID    string   `json:"instanceId"`
}

const a2uiRootID = "root"

func a2uiSurfaceID(sid string) string { return "session:" + sid }

func surfaceUpdate(sid string, components ...a2uiComponent) a2uiMessage {
	return a2uiMessage{SurfaceUpdate: &a2uiSurfaceUpdate{SurfaceID: a2uiSurfaceID(sid), Components: components}}
}

func dataUpdate(sid string, contents ...a2uiContent) a2uiMessage {
	return a2uiMessage{DataModelUpdate: &a2uiDataModelUpdate{SurfaceID: a2uiSurfaceID(sid), Contents: contents}}
}

// render writes the complete current surface as JSONL from one read-only
// snapshot; X-Session-Cursor names the durable position of that view.
func (r *routes) render(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("sid")
	snap, err := r.catalog.Snapshot(req.Context(), sid)
	if err != nil {
		WriteError(w, err)
		return
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, msg := range renderSnapshot(sid, snap, r.catalog.InstanceID()) {
		if err := enc.Encode(msg); err != nil {
			WriteError(w, product.NewError(product.CodeInternal, "render encoding failed"))
			return
		}
	}
	w.Header().Set("Content-Type", "application/jsonl; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Session-Cursor", EncodeCursor(sid, snap.Cursor))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body.Bytes())
}

// approvalEvent reports durable events after which runtime approvals may have
// appeared or disappeared.
func approvalEvent(t string) bool {
	return strings.HasPrefix(t, "trace.") || strings.HasPrefix(t, "tool.")
}

// uiEvents streams A2UI frames: the frame group of one durable event carries
// its cursor only on the last frame, durable events without UI change send an
// id-bearing cursor frame, and temporary frames never carry an id.
func (r *routes) uiEvents(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("sid")
	after, err := streamCursor(req, sid)
	if err != nil {
		WriteError(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, unavailable("streaming unsupported"))
		return
	}
	snap, err := r.catalog.Snapshot(req.Context(), sid)
	if err != nil {
		WriteError(w, err)
		return
	}
	instance := r.catalog.InstanceID()
	known := newUIState(sid, snap, instance)
	sub, live, closeSub, err := r.catalog.Subscribe(req.Context(), sid, after, config.Limits{})
	if err != nil {
		WriteError(w, err)
		return
	}
	defer closeSub()
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	write := func(frame string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(config.WebSSEWriteDeadline))
		if _, err := fmt.Fprint(w, frame); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	frames := func(msgs []a2uiMessage, id string) bool {
		for i, msg := range msgs {
			raw, err := json.Marshal(msg)
			if err != nil {
				return false
			}
			frame := "event: a2ui\n"
			if id != "" && i == len(msgs)-1 {
				frame += "id: " + id + "\n"
			}
			if !write(frame + "data: " + string(raw) + "\n\n") {
				return false
			}
		}
		return true
	}
	if !write(fmt.Sprintf("event: ready\ndata: {\"handoff\":%q,\"live\":%t}\n\n", EncodeCursor(sid, sub.Handoff), live)) {
		return
	}
	heartbeat := time.NewTicker(config.WebSSEHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-req.Context().Done():
			return
		case <-heartbeat.C:
			if !write(": heartbeat\n\n") {
				return
			}
		case ev, ok := <-sub.Events:
			if !ok {
				if pe, isProduct := product.AsError(sub.Err()); isProduct && pe.Code == product.CodeResyncRequired {
					write("event: resync\ndata: {\"code\":\"resync_required\"}\n\n")
				} else if !live {
					write("event: end\ndata: {}\n\n")
				}
				return
			}
			msgs := projectUIEvent(sid, ev, known)
			if ev.DurableSeq == nil {
				if !frames(msgs, "") {
					return
				}
				continue
			}
			id := EncodeCursor(sid, *ev.DurableSeq)
			if len(msgs) == 0 {
				if !write("event: cursor\nid: " + id + "\ndata: {}\n\n") {
					return
				}
			} else if !frames(msgs, id) {
				return
			}
			// Runtime approvals, child invocations and workflow nodes have no
			// events of their own; refresh them after trace/tool facts as
			// temporary frames. render and reconnect rebuild them from snapshots.
			if live && approvalEvent(ev.Type) {
				if current, err := r.catalog.Snapshot(req.Context(), sid); err == nil {
					if known.syncProgress(current) && !frames(known.progressFrames(), "") {
						return
					}
					if known.syncApprovals(current, instance) && !frames(known.approvalFrames(), "") {
						return
					}
				}
			}
		}
	}
}
