package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/workflowagent"
)

type workflowEventDTO struct {
	Type            string          `json:"type"`
	EventID         string          `json:"eventId,omitempty"`
	RunID           string          `json:"runId"`
	NodeExecutionID string          `json:"nodeExecutionId,omitempty"`
	Cursor          string          `json:"cursor,omitempty"`
	OccurredAt      time.Time       `json:"occurredAt"`
	Payload         json.RawMessage `json:"payload,omitempty"`
}

func projectWorkflowEvent(rid string, ev agent.Event) (workflowEventDTO, error) {
	switch ev.Type {
	case "workflow.created", "workflow.input.accepted", "workflow.state_changed", "workflow.node.state_changed",
		"model.attempt.started", "model.attempt.finished", "tool.requested", "tool.finished",
		"message.started", "message.snapshot", "tool.output.delta", "tool.output.snapshot",
		"interaction.requested", "interaction.resolved":
	default:
		return workflowEventDTO{}, product.NewError(product.CodeInternal, "workflow event type is unavailable")
	}
	if ev.Scope.WorkflowRunID != rid || ev.Scope.SessionID != "" || ev.Scope.TraceID != "" || ev.Scope.TurnID != "" {
		return workflowEventDTO{}, product.NewError(product.CodeInternal, "workflow event root differs")
	}
	out := workflowEventDTO{Type: ev.Type, EventID: ev.EventID, RunID: rid, NodeExecutionID: ev.Scope.NodeExecutionID, OccurredAt: ev.OccurredAt}
	if ev.DurableSeq != nil {
		out.Cursor = encodeWorkflowCursor(rid, *ev.DurableSeq)
	}
	// Temporary model/tool/approval payloads are refresh notifications only.
	// The authoritative current question is projected from Snapshot, never a
	// stale stream frame or an internal frozen call.
	if ev.DurableSeq != nil && (ev.Type == "workflow.created" || ev.Type == "workflow.state_changed") {
		var v struct {
			State string `json:"state"`
		}
		if json.Unmarshal(ev.Payload, &v) == nil {
			switch v.State {
			case "created", "running", "pausing", "paused", "cancelling", "cancelled", "completed", "failed":
				out.Payload, _ = json.Marshal(v)
			}
		}
	}
	return out, nil
}
func workflowStreamCursor(req *http.Request) (string, error) {
	values := req.URL.Query()["cursor"]
	if len(values) > 1 {
		return "", invalid("one workflow cursor is required")
	}
	cursor := req.URL.Query().Get("cursor")
	if last := req.Header.Values("Last-Event-ID"); len(last) > 0 {
		if len(last) != 1 || (cursor != "" && cursor != last[0]) {
			return "", invalid("cursor and Last-Event-ID disagree")
		}
		cursor = last[0]
	}
	return cursor, nil
}
func (r *routes) workflowEvents(w http.ResponseWriter, req *http.Request) {
	rid := req.PathValue("rid")
	cursor, err := workflowStreamCursor(req)
	if err != nil {
		WriteError(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, unavailable("streaming unsupported"))
		return
	}
	sub, live, release, err := r.workflows.Subscribe(req.Context(), rid, workflowagent.WorkflowSubscribeOptions{Cursor: cursor, Limits: config.Limits{}})
	if err != nil {
		WriteError(w, err)
		return
	}
	defer release()
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
	if !write(fmt.Sprintf("event: ready\ndata: {\"handoff\":%q,\"live\":%t}\n\n", encodeWorkflowCursor(rid, sub.Handoff), live)) {
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
			dto, err := projectWorkflowEvent(rid, ev)
			if err != nil {
				write("event: resync\ndata: {\"code\":\"resync_required\"}\n\n")
				return
			}
			raw, err := json.Marshal(dto)
			if err != nil {
				return
			}
			frame := "event: event\n"
			if ev.DurableSeq != nil {
				frame += "id: " + dto.Cursor + "\n"
			}
			if !write(frame + "data: " + string(raw) + "\n\n") {
				return
			}
		}
	}
}
