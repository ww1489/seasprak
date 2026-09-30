package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/testkit"
)

type rawResponse struct {
	status int
	header http.Header
	body   []byte
}

func (r rawResponse) code() string {
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(r.body, &out)
	return out.Error.Code
}

func rawCall(t *testing.T, s *Server, token, method, path, key, contentType string, body []byte, header map[string]string) rawResponse {
	t.Helper()
	req, _ := http.NewRequest(method, s.URL()+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return rawResponse{status: resp.StatusCode, header: resp.Header, body: raw}
}

func upload(t *testing.T, s *Server, token, sid, key, mimeType, name string, body []byte) (rawResponse, map[string]any) {
	t.Helper()
	path := "/v1/sessions/" + sid + "/attachments"
	if name != "" {
		path += "?name=" + name
	}
	r := rawCall(t, s, token, "POST", path, key, mimeType, body, nil)
	var out map[string]any
	_ = json.Unmarshal(r.body, &out)
	return r, out
}

// startAt starts the real routes over an existing config (same state root).
func startAt(t *testing.T, c Config, model *testkit.FakeModel) (*Server, string) {
	t.Helper()
	testOptions = func(o *sessions.Options) { o.Model = model }
	defer func() { testOptions = nil }()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, c, nil)
	if err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { cancel(); _ = s.Wait() })
	token, err := os.ReadFile(s.TokenPath())
	if err != nil {
		t.Fatal("token")
	}
	return s, string(token)
}

func TestAttachmentSaveReadAndBoundaries(t *testing.T) {
	s, token, c, model := offlineServer(t)
	sid := createSession(t, s, token, c)
	content := []byte("hello attachment")
	r, saved := upload(t, s, token, sid, "a1", "text/plain; charset=utf-8", "../../notes.txt", content)
	aid, _ := saved["artifactId"].(string)
	if r.status != 201 || aid == "" || saved["mimeType"] != "text/plain" || saved["size"] != float64(len(content)) {
		t.Fatalf("save status=%d %s", r.status, r.body)
	}
	if strings.Contains(aid, "notes") || strings.ContainsAny(aid, `/\.`) {
		t.Fatal("artifact id derived from display name")
	}
	if again, replay := upload(t, s, token, sid, "a1", "text/plain", "../../notes.txt", content); again.status != 200 || replay["artifactId"] != aid {
		t.Fatalf("replay status=%d", again.status)
	}
	if conflict, _ := upload(t, s, token, sid, "a1", "text/plain", "", []byte("other")); conflict.status != 409 || conflict.code() != "idempotency_conflict" {
		t.Fatalf("changed replay status=%d", conflict.status)
	}

	get := "/v1/sessions/" + sid + "/attachments/" + aid
	full := rawCall(t, s, token, "GET", get, "", "", nil, nil)
	if full.status != 200 || !bytes.Equal(full.body, content) || full.header.Get("Content-Type") != "text/plain" || full.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("full read status=%d headers=%v", full.status, full.header)
	}
	if cd := full.header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || strings.ContainsAny(strings.TrimPrefix(cd, "attachment"), `/\`) {
		t.Fatalf("disposition=%q", cd)
	}
	part := rawCall(t, s, token, "GET", get, "", "", nil, map[string]string{"Range": "bytes=2-5"})
	if part.status != 206 || string(part.body) != string(content[2:6]) || part.header.Get("Content-Range") != "bytes 2-5/16" {
		t.Fatalf("range status=%d body=%q cr=%q", part.status, part.body, part.header.Get("Content-Range"))
	}
	suffix := rawCall(t, s, token, "GET", get, "", "", nil, map[string]string{"Range": "bytes=-3"})
	if suffix.status != 206 || string(suffix.body) != "ent" {
		t.Fatalf("suffix range status=%d body=%q", suffix.status, suffix.body)
	}
	for _, bad := range []string{"bytes=100-200", "bytes=5-2", "bytes=0-1,3-4", "items=0-1", "bytes=x-1"} {
		if got := rawCall(t, s, token, "GET", get, "", "", nil, map[string]string{"Range": bad}); got.status != 416 || got.header.Get("Content-Range") != "bytes */16" {
			t.Fatalf("range %q status=%d", bad, got.status)
		}
	}

	for name, tc := range map[string]struct {
		mime   string
		body   []byte
		status int
		code   string
	}{
		"disallowed mime": {"text/html", []byte("<p>x</p>"), 422, "unsupported_capability"},
		"png mismatch":    {"image/png", []byte("not a png"), 400, "invalid_argument"},
		"jpeg is png":     {"image/jpeg", []byte("\x89PNG\r\n\x1a\n0000"), 400, "invalid_argument"},
		"binary as text":  {"text/plain", []byte{0, 1, 2, 0xff}, 400, "invalid_argument"},
		"bad json":        {"application/json", []byte("{"), 400, "invalid_argument"},
		"empty":           {"text/plain", nil, 400, "invalid_argument"},
		"missing type":    {"", []byte("x"), 400, "invalid_argument"},
	} {
		if got, _ := upload(t, s, token, sid, "bad-"+name, tc.mime, "", tc.body); got.status != tc.status || got.code() != tc.code {
			t.Fatalf("%s status=%d code=%s", name, got.status, got.code())
		}
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	if got, out := upload(t, s, token, sid, "png", "image/png", "pic.png", png); got.status != 201 || out["mimeType"] != "image/png" {
		t.Fatalf("png status=%d %s", got.status, got.body)
	}
	oversize := bytes.Repeat([]byte("a"), config.WebAttachmentBytes+1)
	if got, _ := upload(t, s, token, sid, "big", "text/plain", "", oversize); got.status != 413 || got.code() != "invalid_argument" {
		t.Fatalf("oversize status=%d code=%s", got.status, got.code())
	}
	if got := rawCall(t, s, token, "POST", "/v1/sessions/"+sid+"/attachments", "", "text/plain", []byte("x"), nil); got.status != 400 {
		t.Fatalf("missing key status=%d", got.status)
	}

	status, other := call(t, s, token, "POST", "/v1/sessions", "create-2", map[string]any{"workspace": c.Workspace})
	sid2, _ := other["sessionId"].(string)
	if status != 201 || sid2 == sid {
		t.Fatalf("second session status=%d", status)
	}
	if got := rawCall(t, s, token, "GET", "/v1/sessions/"+sid2+"/attachments/"+aid, "", "", nil, nil); got.status != 404 {
		t.Fatalf("cross-session status=%d", got.status)
	}
	for _, bad := range []string{"..%2F..%2Fjournal.jsonl", aid + ".json", strings.ToUpper(aid), "00000000000000000000000000000000", "..%5Cjournal.jsonl"} {
		if got := rawCall(t, s, token, "GET", "/v1/sessions/"+sid+"/attachments/"+bad, "", "", nil, nil); got.status != 404 {
			t.Fatalf("id %q status=%d", bad, got.status)
		}
	}
	if got, _ := upload(t, s, token, "missing", "k", "text/plain", "", []byte("x")); got.status != 404 {
		t.Fatalf("unknown session status=%d", got.status)
	}
	if model.Calls() != 0 {
		t.Fatalf("attachments invoked the model %d times", model.Calls())
	}
	if _, snap := call(t, s, token, "GET", "/v1/sessions/"+sid+"/snapshot", "", nil); len(snap["traces"].([]any)) != 0 {
		t.Fatal("attachment created a trace")
	}
}

func TestAttachmentSurvivesRestartAndCountLimit(t *testing.T) {
	c := testConfig(t)
	setProfile(t, c, "memory")
	model := testkit.NewFake()
	s, token := startAt(t, c, model)
	sid := createSession(t, s, token, c)
	_, saved := upload(t, s, token, sid, "keep", "text/markdown", "a.md", []byte("# title"))
	aid, _ := saved["artifactId"].(string)
	for i := 1; i < config.WebAttachmentsPerSession; i++ {
		if got, _ := upload(t, s, token, sid, "n"+strings.Repeat("x", i), "text/plain", "", []byte("x")); got.status != 201 {
			t.Fatalf("upload %d status=%d", i, got.status)
		}
	}
	if got, _ := upload(t, s, token, sid, "one-more", "text/plain", "", []byte("x")); got.status != 422 || got.code() != "budget_exhausted" {
		t.Fatalf("count limit status=%d code=%s", got.status, got.code())
	}
	// A replay of an existing key still succeeds at the limit.
	if got, _ := upload(t, s, token, sid, "keep", "text/markdown", "a.md", []byte("# title")); got.status != 200 {
		t.Fatalf("replay at limit status=%d", got.status)
	}
	s.Close()
	if err := s.Wait(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	s2, token2 := startAt(t, c, model)
	got := rawCall(t, s2, token2, "GET", "/v1/sessions/"+sid+"/attachments/"+aid, "", "", nil, nil)
	if got.status != 200 || string(got.body) != "# title" || got.header.Get("Content-Type") != "text/markdown" {
		t.Fatalf("after restart status=%d body=%q", got.status, got.body)
	}
	if model.Calls() != 0 {
		t.Fatal("attachments invoked the model")
	}
}

func TestMetadataRoutes(t *testing.T) {
	s, token, c, model := offlineServer(t)
	sid := createSession(t, s, token, c)
	journal := s.SessionOptions().StateRoot + "/sessions/" + sid + "/journal.jsonl"
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal("journal")
	}
	if status, m := call(t, s, token, "GET", "/v1/sessions/"+sid+"/metadata", "", nil); status != 200 || m["name"] != "" || len(m["labels"].([]any)) != 0 {
		t.Fatalf("empty metadata status=%d %v", status, m)
	}
	body := map[string]any{"name": "Refactor", "labels": []string{"p3", "web"}}
	status, m := call(t, s, token, "PATCH", "/v1/sessions/"+sid+"/metadata", "m1", body)
	if status != 200 || m["name"] != "Refactor" || m["revision"] != float64(1) {
		t.Fatalf("set status=%d %v", status, m)
	}
	if status, again := call(t, s, token, "PATCH", "/v1/sessions/"+sid+"/metadata", "m1", body); status != 200 || again["revision"] != float64(1) {
		t.Fatalf("replay status=%d %v", status, again)
	}
	if status, _ = call(t, s, token, "PATCH", "/v1/sessions/"+sid+"/metadata", "m1", map[string]any{"name": "Other", "labels": []string{}}); status != 409 {
		t.Fatalf("conflict status=%d", status)
	}
	if status, _ = call(t, s, token, "PUT", "/v1/sessions/"+sid+"/metadata", "", body); status != 400 {
		t.Fatalf("missing key status=%d", status)
	}
	for name, bad := range map[string]any{
		"unknown field": map[string]any{"name": "x", "labels": []string{}, "sessionId": "y"},
		"dup label":     map[string]any{"name": "x", "labels": []string{"a", "a"}},
		"control":       map[string]any{"name": "a\nb", "labels": []string{}},
		"long name":     map[string]any{"name": strings.Repeat("n", config.SessionNameBytes+1), "labels": []string{}},
	} {
		if status, _ = call(t, s, token, "PUT", "/v1/sessions/"+sid+"/metadata", "bad-"+name, bad); status != 400 {
			t.Fatalf("%s status=%d", name, status)
		}
	}
	if status, m = call(t, s, token, "PUT", "/v1/sessions/"+sid+"/metadata", "m2", map[string]any{"name": "Final", "labels": []string{"done"}}); status != 200 || m["revision"] != float64(2) {
		t.Fatalf("put status=%d %v", status, m)
	}
	_, list := call(t, s, token, "GET", "/v1/sessions", "", nil)
	entry := list["sessions"].([]any)[0].(map[string]any)
	if entry["name"] != "Final" || len(entry["labels"].([]any)) != 1 {
		t.Fatalf("list entry=%v", entry)
	}
	if _, got := call(t, s, token, "GET", "/v1/sessions/"+sid, "", nil); got["name"] != "Final" {
		t.Fatalf("get entry=%v", got)
	}
	if status, _ = call(t, s, token, "PATCH", "/v1/sessions/missing/metadata", "k", body); status != 404 {
		t.Fatalf("unknown session status=%d", status)
	}
	after, _ := os.ReadFile(journal)
	if !bytes.Equal(before, after) || model.Calls() != 0 {
		t.Fatal("metadata wrote the journal or invoked the model")
	}
}

func TestWorkflowRoute(t *testing.T) {
	s, token, c, _ := offlineServer(t)
	sid := createSession(t, s, token, c)
	if status, out := call(t, s, token, "GET", "/v1/sessions/"+sid+"/workflows", "", nil); status != 200 || len(out["workflows"].([]any)) != 0 {
		t.Fatalf("empty workflows status=%d %v", status, out)
	}

	wf, err := agent.CompileWorkflow(agent.WorkflowDefinition{
		Name: "echo-flow", Version: "v1", Description: "echo", Source: "test", FormatVersion: agent.WorkflowFormatV1,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}`),
		Nodes: []agent.WorkflowNode{
			{ID: "s", Type: agent.WorkflowNodeStart},
			{ID: "e", Type: agent.WorkflowNodeEnd, Inputs: map[string]agent.WorkflowValue{"answer": {Ref: &agent.WorkflowRef{Node: "s", Field: "topic"}}}},
		},
		Edges: []agent.WorkflowEdge{{From: "s", To: "e"}},
	}, agent.WorkflowBindings{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	c2 := testConfig(t)
	setProfile(t, c2, "memory")
	model := testkit.NewFake()
	testOptions = func(o *sessions.Options) {
		o.Model = model
		o.Agents = []agent.AgentDefinition{{Name: "echo-flow", Version: "v1", Description: "echo", Kind: agent.AgentKindWorkflow, Workflow: wf}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s2, err := Start(ctx, c2, nil)
	testOptions = nil
	if err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { cancel(); _ = s2.Wait() })
	raw, _ := os.ReadFile(s2.TokenPath())
	token2 := string(raw)
	sid2 := createSession(t, s2, token2, c2)
	status, out := call(t, s2, token2, "GET", "/v1/sessions/"+sid2+"/workflows", "", nil)
	list, _ := out["workflows"].([]any)
	if status != 200 || len(list) != 1 {
		t.Fatalf("workflows status=%d %v", status, out)
	}
	item := list[0].(map[string]any)
	if item["name"] != "echo-flow" || item["version"] != "v1" || item["description"] != "echo" || item["inputSchema"] == nil {
		t.Fatalf("workflow item=%v", item)
	}
	for _, private := range []string{"nodes", "edges", "hash", "source"} {
		if _, ok := item[private]; ok {
			t.Fatalf("workflow exposed %s", private)
		}
	}
	if status, _ = call(t, s2, token2, "GET", "/v1/sessions/missing/workflows", "", nil); status != 404 {
		t.Fatalf("unknown session status=%d", status)
	}
	if model.Calls() != 0 {
		t.Fatal("workflow listing invoked the model")
	}
}

// An input may reference a saved text attachment by ID; mixing fields in one
// block or unknown block types is rejected before any model call.
func TestInputReferencesAttachmentOverHTTP(t *testing.T) {
	s, token, c, model := offlineServer(t)
	sid := createSession(t, s, token, c)
	_, saved := upload(t, s, token, sid, "a1", "text/plain", "n.txt", []byte("attached"))
	aid, _ := saved["artifactId"].(string)
	for _, bad := range [][]map[string]string{
		{{"type": "attachment", "artifactId": aid, "text": "x"}},
		{{"type": "image", "artifactId": aid}},
		{{"type": "attachment"}},
	} {
		if status, _ := call(t, s, token, "POST", "/v1/sessions/"+sid+"/inputs", "bad-"+bad[0]["type"]+bad[0]["text"], map[string]any{"kind": "prompt", "content": bad}); status != 400 {
			t.Fatalf("bad block %v status=%d", bad, status)
		}
	}
	status, receipt := call(t, s, token, "POST", "/v1/sessions/"+sid+"/inputs", "ok", map[string]any{"kind": "prompt", "content": []map[string]string{{"type": "text", "text": "read it"}, {"type": "attachment", "artifactId": aid}}})
	if status != 202 {
		t.Fatalf("input status=%d %v", status, receipt)
	}
	tid, _ := receipt["traceId"].(string)
	pollSnapshot(t, s, token, sid, func(m map[string]any) bool { return traceState(m, tid) == "completed" })
	if model.Calls() != 1 {
		t.Fatalf("model calls=%d", model.Calls())
	}
}
