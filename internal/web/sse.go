package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

// eventDTO is the public SSE projection of a product event. Durable event
// payloads are internal records, so only whitelisted fields are forwarded.
type eventDTO struct {
	Type       string          `json:"type"`
	EventID    string          `json:"eventId,omitempty"`
	TraceID    string          `json:"traceId,omitempty"`
	TurnID     string          `json:"turnId,omitempty"`
	Cursor     string          `json:"cursor,omitempty"`
	StreamID   string          `json:"streamId,omitempty"`
	ChunkSeq   *uint64         `json:"chunkSeq,omitempty"`
	OccurredAt time.Time       `json:"occurredAt"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

func projectEvent(sid string, ev agent.Event) eventDTO {
	out := eventDTO{Type: ev.Type, EventID: ev.EventID, TraceID: ev.Scope.TraceID, TurnID: ev.Scope.TurnID, StreamID: ev.StreamID, ChunkSeq: ev.ChunkSeq, OccurredAt: ev.OccurredAt}
	if ev.DurableSeq != nil {
		out.Cursor = EncodeCursor(sid, *ev.DurableSeq)
	}
	out.Payload = publicPayload(ev)
	return out
}

func publicPayload(ev agent.Event) json.RawMessage {
	var v any
	switch ev.Type {
	case "trace.state_changed", "trace.settled":
		var tr struct {
			ID      string `json:"id"`
			State   string `json:"state"`
			Settled bool   `json:"settled"`
			Hold    bool   `json:"hold"`
		}
		if json.Unmarshal(ev.Payload, &tr) != nil {
			return nil
		}
		v = map[string]any{"traceId": tr.ID, "state": tr.State, "settled": tr.Settled, "hold": tr.Hold}
	case "input.accepted":
		var r agent.InputReceipt
		if json.Unmarshal(ev.Payload, &r) != nil {
			return nil
		}
		v = map[string]any{"inputId": r.InputID, "traceId": r.TraceID, "actualKind": r.ActualKind, "state": r.State}
	case "message.finalized":
		var m agent.AgentMessage
		if json.Unmarshal(ev.Payload, &m) != nil {
			return nil
		}
		v = projectMessage(m)
	case "message.snapshot":
		var s agent.ModelStreamSnapshot
		if json.Unmarshal(ev.Payload, &s) != nil {
			return nil
		}
		blocks := []blockDTO{}
		for _, b := range s.Blocks {
			if b.Type == "text" || b.Type == "tool_call" {
				blocks = append(blocks, blockDTO{Type: b.Type, Text: b.Text, CallID: b.CallID, Name: b.Name})
			}
		}
		v = map[string]any{"messageId": s.MessageID, "blocks": blocks}
	case "tool.output.delta":
		var d agent.ToolOutputDelta
		if json.Unmarshal(ev.Payload, &d) != nil {
			return nil
		}
		v = d
	default:
		// Other types expose identity and position only.
		return nil
	}
	raw, _ := json.Marshal(v)
	return raw
}

// streamCursor resolves the resume position from ?cursor= and Last-Event-ID;
// when both are present they must be identical.
func streamCursor(req *http.Request, sid string) (uint64, error) {
	cursor := req.URL.Query().Get("cursor")
	if last := req.Header.Values("Last-Event-ID"); len(last) > 0 {
		if len(last) != 1 || (cursor != "" && cursor != last[0]) {
			return 0, invalid("cursor and Last-Event-ID disagree")
		}
		cursor = last[0]
	}
	return DecodeCursor(sid, cursor)
}

// events serves text/event-stream: durable replay after the cursor, then live
// events. Temporary events carry no SSE id. The stream ends with a resync
// frame on overflow; a client write failure only closes this subscription.
func (r *routes) events(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("sid")
	after, err := streamCursor(req, sid)
	if err != nil {
		WriteError(w, err)
		return
	}
	traceFilter := req.URL.Query().Get("traceId")
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, unavailable("streaming unsupported"))
		return
	}
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
			if traceFilter != "" && ev.Scope.TraceID != traceFilter {
				continue
			}
			dto := projectEvent(sid, ev)
			raw, err := json.Marshal(dto)
			if err != nil {
				return
			}
			frame := "event: " + ev.Type + "\n"
			if ev.DurableSeq != nil {
				frame += "id: " + dto.Cursor + "\n"
			}
			if !write(frame + "data: " + string(raw) + "\n\n") {
				return
			}
		}
	}
}
