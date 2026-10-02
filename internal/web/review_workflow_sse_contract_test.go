package web

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The browser consumes product facts as SSE "event" frames. The product fact's
// own type stays in the public JSON payload; it is not the SSE dispatch name.
func TestReviewWorkflowSSEUsesConsumerFrameEvent(t *testing.T) {
	conf := testConfig(t)
	configureHTTPWorkflow(t, conf, httpLiteralWorkflow())
	server, token, model := workflowHTTPServer(t, conf)
	status, initial := call(t, server, token, "POST", "/v1/workflow-runs", "frame-contract", workflowCreateBody(conf))
	if status != http.StatusAccepted {
		t.Fatalf("create status=%d", status)
	}
	rid, ok := initial["runId"].(string)
	if !ok || rid == "" {
		t.Fatal("create did not return an independent run ID")
	}
	pollWorkflowHTTP(t, server, token, rid, func(snapshot map[string]any) bool {
		return snapshot["state"] == "completed" && snapshot["executionStopped"] == true
	})
	server.Close()
	if err := server.Wait(); err != nil {
		t.Fatal(err)
	}
	server, token, reopenedModel := workflowHTTPServer(t, conf)
	frames, status := readFrames(t, server, token, "/v1/workflow-runs/"+rid+"/events", "", func(frames []sseFrame) bool {
		return len(frames) != 0 && frames[len(frames)-1].event == "end"
	})
	if status != http.StatusOK {
		t.Fatalf("history stream status=%d", status)
	}
	facts := 0
	for _, frame := range frames {
		if frame.event == "ready" || frame.event == "end" {
			continue
		}
		var fact struct {
			Type  string `json:"type"`
			RunID string `json:"runId"`
		}
		if err := json.Unmarshal([]byte(frame.data), &fact); err != nil {
			t.Fatalf("product frame is not JSON: dispatch=%s", frame.event)
		}
		if frame.event != "event" {
			t.Errorf("browser ignores product fact: SSE dispatch=%q, payload type=%q; want dispatch=event", frame.event, fact.Type)
		}
		if fact.Type == "" || fact.RunID != rid || frame.id == "" {
			t.Errorf("durable fact lost its type, independent root, or cursor: dispatch=%s", frame.event)
		}
		facts++
	}
	if facts == 0 || model.Calls() != 0 || reopenedModel.Calls() != 0 {
		t.Fatalf("history did not exercise independent facts with zero Code model calls: facts=%d calls=%d/%d", facts, model.Calls(), reopenedModel.Calls())
	}
}
