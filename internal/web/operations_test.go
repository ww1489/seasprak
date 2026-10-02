package web

import (
	"strings"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/codeagent"
	"github.com/ww1489/seasprak/internal/testkit"
)

// offlineServer starts the real routes with a scripted offline model.
func offlineServer(t *testing.T, steps ...testkit.Step) (*Server, string, Config, *testkit.FakeModel) {
	t.Helper()
	model := testkit.NewFake(steps...)
	testOptions = func(o *codeagent.Options) { o.Model = model }
	t.Cleanup(func() { testOptions = nil })
	s, token, c := routeServer(t, "memory")
	testOptions = nil
	return s, token, c, model
}

func createSession(t *testing.T, s *Server, token string, c Config) string {
	t.Helper()
	status, out := call(t, s, token, "POST", "/v1/sessions", "create", map[string]any{"workspace": c.Workspace})
	sid, _ := out["sessionId"].(string)
	if status != 201 || sid == "" {
		t.Fatalf("create status=%d", status)
	}
	return sid
}

func pollSnapshot(t *testing.T, s *Server, token, sid string, done func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, snap := call(t, s, token, "GET", "/v1/sessions/"+sid+"/snapshot", "", nil)
		if done(snap) {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot condition not reached: %v", snap)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func traceState(snap map[string]any, id string) string {
	traces, _ := snap["traces"].([]any)
	for _, raw := range traces {
		tr, _ := raw.(map[string]any)
		if tr["traceId"] == id {
			s, _ := tr["state"].(string)
			return s
		}
	}
	return ""
}

func TestInputRouteRunsTypedTextAndReplays(t *testing.T) {
	s, token, c, model := offlineServer(t, testkit.Step{Text: "hello back"})
	sid := createSession(t, s, token, c)
	body := map[string]any{"kind": "prompt", "content": []map[string]string{{"type": "text", "text": "hello"}}}
	status, receipt := call(t, s, token, "POST", "/v1/sessions/"+sid+"/inputs", "in1", body)
	tid, _ := receipt["traceId"].(string)
	if status != 202 || tid == "" || receipt["actualKind"] != "prompt" {
		t.Fatalf("input status=%d %v", status, receipt)
	}
	snap := pollSnapshot(t, s, token, sid, func(m map[string]any) bool { return traceState(m, tid) == "completed" })
	msgs, _ := snap["messages"].([]any)
	joined := ""
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		blocks, _ := m["blocks"].([]any)
		for _, b := range blocks {
			bm, _ := b.(map[string]any)
			text, _ := bm["text"].(string)
			joined += text + "|"
		}
	}
	// The typed block becomes real user text, not a serialized JSON object.
	if !strings.Contains(joined, "hello|") || strings.Contains(joined, `{"text"`) || !strings.Contains(joined, "hello back") {
		t.Fatalf("messages=%q", joined)
	}
	again, replay := call(t, s, token, "POST", "/v1/sessions/"+sid+"/inputs", "in1", body)
	if again != 202 || replay["traceId"] != tid || model.Calls() != 1 {
		t.Fatalf("replay status=%d calls=%d", again, model.Calls())
	}
	for name, bad := range map[string]any{
		"image":   map[string]any{"kind": "prompt", "content": []map[string]string{{"type": "image", "text": "x"}}},
		"empty":   map[string]any{"kind": "prompt", "content": []map[string]string{}},
		"role":    map[string]any{"kind": "prompt", "role": "system", "content": []map[string]string{{"type": "text", "text": "x"}}},
		"command": map[string]any{"kind": "command", "content": []map[string]string{{"type": "text", "text": "rm"}}},
	} {
		if status, _ = call(t, s, token, "POST", "/v1/sessions/"+sid+"/inputs", "bad-"+name, bad); status < 400 || status >= 500 {
			t.Fatalf("%s accepted with status=%d", name, status)
		}
	}
	if model.Calls() != 1 {
		t.Fatal("rejected input reached the model")
	}
}

func TestCancelRouteAcceptsBeforeStopAndQueriesOperation(t *testing.T) {
	gate := make(chan struct{})
	s, token, c, model := offlineServer(t, testkit.Step{Gate: gate})
	sid := createSession(t, s, token, c)
	_, receipt := call(t, s, token, "POST", "/v1/sessions/"+sid+"/inputs", "in", map[string]any{"kind": "prompt", "content": []map[string]string{{"type": "text", "text": "block"}}})
	tid, _ := receipt["traceId"].(string)
	deadline := time.Now().Add(5 * time.Second)
	for model.Calls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("model not called")
		}
		time.Sleep(time.Millisecond)
	}
	status, op := call(t, s, token, "POST", "/v1/sessions/"+sid+"/traces/"+tid+"/cancel", "c", map[string]any{})
	oid, _ := op["operationId"].(string)
	if status != 202 || oid == "" || op["scope"] != "durable" {
		t.Fatalf("cancel status=%d %v", status, op)
	}
	pollSnapshot(t, s, token, sid, func(m map[string]any) bool { return traceState(m, tid) == "cancelled" })
	deadline = time.Now().Add(5 * time.Second)
	for {
		status, got := call(t, s, token, "GET", "/v1/sessions/"+sid+"/operations/"+oid, "", nil)
		if status == 200 && got["state"] == "completed" && got["result"] == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation not completed: %d %v", status, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status, _ = call(t, s, token, "POST", "/v1/sessions/"+sid+"/traces/"+tid+"/cancel", "", map[string]any{}); status != 400 {
		t.Fatalf("missing key status=%d", status)
	}
	if status, _ = call(t, s, token, "POST", "/v1/sessions/"+sid+"/interactions/x/responses", "r", map[string]any{"decision": "allowed-once", "expectedRevision": 1, "instanceId": "stale"}); status != 409 {
		t.Fatalf("stale instance approval status=%d", status)
	}
	if status, _ = call(t, s, token, "POST", "/v1/sessions/"+sid+"/traces/"+tid+"/reconcile", "q", map[string]any{"grantRef": "forged"}); status != 400 {
		t.Fatalf("client grantRef status=%d", status)
	}
	if model.Calls() != 1 {
		t.Fatal("cancel executed more model calls")
	}
}
