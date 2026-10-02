package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestWorkflowEventProjectionRejectsUnknownTypeAndCrossRoot(t *testing.T) {
	seq := uint64(1)
	event := agent.Event{Type: "workflow.state_changed", Scope: agent.EventScope{WorkflowRunID: "run"}, EventID: "event", DurableSeq: &seq, Payload: json.RawMessage(`{"state":"paused","arguments":"synthetic-private-marker","providerExtra":"synthetic-private-marker"}`)}
	dto, err := projectWorkflowEvent("run", event)
	raw, _ := json.Marshal(dto)
	if err != nil || strings.Contains(string(raw), "synthetic-private-marker") || dto.Cursor != encodeWorkflowCursor("run", 1) {
		t.Fatal("workflow event projection leaked payload or lost typed cursor")
	}
	bads := []agent.Event{event, event, event, event, event}
	bads[0].Scope.WorkflowRunID = "other"
	bads[1].Scope.SessionID = "run"
	bads[2].Scope.TraceID = "trace"
	bads[3].Scope.TurnID = "turn"
	bads[4].Type = "private-provider-frame\nsynthetic-private-marker"
	for _, bad := range bads {
		if _, err := projectWorkflowEvent("run", bad); err == nil {
			t.Fatalf("cross root or unknown type applied: type=%q scope=%+v", bad.Type, bad.Scope)
		} else if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInternal {
			t.Fatalf("projection error=%v", err)
		}
	}
}
func TestWorkflowHTTPSSEAtomicHandoffTypedCursorAndPrivacy(t *testing.T) {
	entered, emit := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-emit:
		default:
			close(emit)
		}
	}()
	var effects atomic.Int32
	tool := tools.Definition{Name: "probe", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "none"}, RunWithOutput: func(ctx context.Context, _ json.RawMessage, sink agent.ToolOutputSink) (string, error) {
		effects.Add(1)
		if err := sink.WriteOutput(ctx, agent.ToolOutputChunk{Stream: "stdout", Text: "synthetic-private-output"}); err != nil {
			return "", err
		}
		close(entered)
		<-emit
		for range 3 {
			if err := sink.WriteOutput(ctx, agent.ToolOutputChunk{Stream: "stderr", Text: "synthetic-private-output"}); err != nil {
				return "", err
			}
		}
		return "synthetic-private-result", nil
	}}
	runtime, conf := workflowRuntimeFixture(t, workflowToolDefinition("probe"), tool)
	catalogs, server, token := runtimeHTTP(t, runtime)
	status, created := call(t, server, token, "POST", "/v1/workflow-runs", "stream", toolCreateBody(conf))
	if status != 202 {
		t.Fatalf("create status=%d", status)
	}
	rid := created["runId"].(string)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not emit")
	}
	var once sync.Once
	frames, status := readFrames(t, server, token, "/v1/workflow-runs/"+rid+"/events", "", func(frames []sseFrame) bool {
		if strings.Contains(frames[len(frames)-1].data, `"type":"tool.output.snapshot"`) {
			once.Do(func() { close(emit) })
		}
		return strings.Contains(frames[len(frames)-1].data, `"state":"completed"`)
	})
	if status != 200 || len(frames) < 5 || !strings.Contains(frames[0].data, `"live":true`) {
		t.Fatalf("live SSE status=%d frames=%d", status, len(frames))
	}
	final, err := catalogs.workflows.Snapshot(t.Context(), rid)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	temporary := 0
	for _, frame := range frames {
		if frame.event != "ready" && frame.event != "event" {
			t.Fatalf("product frame is not consumable by frontend: %s", frame.event)
		}
		if strings.Contains(frame.data, "synthetic-private") || strings.Contains(frame.data, "private-argument-marker") || strings.Contains(frame.data, "toolCallId") || strings.Contains(frame.data, "sessionId") || strings.Contains(frame.data, "providerExtra") {
			t.Fatalf("SSE leaked private data: %s", frame.data)
		}
		if frame.id != "" {
			ids = append(ids, frame.id)
			if frame.id != encodeWorkflowCursor(rid, uint64(len(ids))) {
				t.Fatalf("handoff duplicate or gap at %d", len(ids))
			}
		} else if frame.event != "ready" {
			temporary++
			if strings.Contains(frame.data, `"cursor"`) {
				t.Fatal("temporary frame got durable cursor")
			}
		}
	}
	if uint64(len(ids)) != final.DurableSeq || temporary == 0 || effects.Load() != 1 {
		t.Fatalf("durable/temporary/effects=%d/%d/%d expected durable=%d", len(ids), temporary, effects.Load(), final.DurableSeq)
	}
	resumed, status := readFrames(t, server, token, "/v1/workflow-runs/"+rid+"/events", ids[1], func(frames []sseFrame) bool {
		n := 0
		for _, frame := range frames {
			if frame.id != "" {
				n++
			}
		}
		return n == len(ids)-2
	})
	got := []string{}
	for _, frame := range resumed {
		if frame.id != "" {
			got = append(got, frame.id)
		}
	}
	if status != 200 || !reflect.DeepEqual(got, ids[2:]) {
		t.Fatal("cursor did not resume exact durable suffix")
	}
	for _, bad := range []string{"garbage", EncodeCursor(rid, 1), encodeWorkflowCursor("other", 1), encodeWorkflowCursor(rid, final.DurableSeq+1), encodeWorkflowCursor(rid, 0) + "="} {
		_, status := readFrames(t, server, token, "/v1/workflow-runs/"+rid+"/events?cursor="+bad, "", func([]sseFrame) bool { return true })
		if status != 400 {
			t.Fatalf("invalid typed cursor status=%d", status)
		}
	}
	_, status = readFrames(t, server, token, "/v1/workflow-runs/"+rid+"/events?cursor="+ids[0], ids[1], func([]sseFrame) bool { return true })
	if status != 400 {
		t.Fatal("conflicting resume positions accepted")
	}
	_, status = readFrames(t, server, token, "/v1/workflow-runs/"+rid+"/events?cursor="+ids[0]+"&cursor="+ids[0], "", func([]sseFrame) bool { return true })
	if status != 400 {
		t.Fatal("multiple cursor values accepted")
	}
}

type workflowSlowWriter struct {
	*httptest.ResponseRecorder
	writes           int
	blocked, release chan struct{}
}

func (w *workflowSlowWriter) Write(raw []byte) (int, error) {
	w.writes++
	if w.writes == 2 {
		close(w.blocked)
		<-w.release
	}
	return w.ResponseRecorder.Write(raw)
}
func TestWorkflowHTTPSSESlowObserverResyncDoesNotBlockExecution(t *testing.T) {
	emit := make(chan struct{})
	defer func() {
		select {
		case <-emit:
		default:
			close(emit)
		}
	}()
	var count atomic.Int32
	tool := tools.Definition{Name: "probe", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "none"}, RunWithOutput: func(ctx context.Context, _ json.RawMessage, sink agent.ToolOutputSink) (string, error) {
		count.Add(1)
		<-emit
		for i := range config.DefaultLimits().SubscriptionEvents + 2 {
			if err := sink.WriteOutput(ctx, agent.ToolOutputChunk{Stream: "stdout", Text: "private-overflow-" + strconv.Itoa(i)}); err != nil {
				return "", err
			}
		}
		return "private-result", nil
	}}
	runtime, conf := workflowRuntimeFixture(t, workflowToolDefinition("probe"), tool)
	catalogs, server, token := runtimeHTTP(t, runtime)
	status, created := call(t, server, token, "POST", "/v1/workflow-runs", "overflow", toolCreateBody(conf))
	if status != 202 {
		t.Fatalf("create status=%d", status)
	}
	rid := created["runId"].(string)
	request := httptest.NewRequest("GET", "/v1/workflow-runs/"+rid+"/events", nil)
	request = request.WithContext(context.WithValue(request.Context(), principalKey{}, localPrincipal))
	writer := &workflowSlowWriter{ResponseRecorder: httptest.NewRecorder(), blocked: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-writer.release:
		default:
			close(writer.release)
		}
	}()
	done := make(chan struct{})
	go func() {
		newRoutes(catalogs.code, localPrincipal, catalogs.workflows).ServeHTTP(writer, request)
		close(done)
	}()
	select {
	case <-writer.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP observer did not block its real write")
	}
	close(emit)
	final := pollWorkflowHTTP(t, server, token, rid, func(v map[string]any) bool { return v["state"] == "completed" && v["executionStopped"] == true })
	if count.Load() != 1 || final["runId"] != rid {
		t.Fatal("slow observer blocked or repeated producer")
	}
	select {
	case <-done:
		t.Fatal("observer bypassed blocked write")
	default:
	}
	close(writer.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("slow subscription did not exit after release")
	}
	raw := writer.Body.String()
	if !strings.Contains(raw, "event: resync") || strings.Contains(raw, "private-overflow") || strings.Contains(raw, "private-result") {
		t.Fatal("slow observer failed resync/privacy")
	}
}
