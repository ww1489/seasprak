package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/testkit"
)

func setProfile(t *testing.T, c Config, profile string) {
	t.Helper()
	b, err := os.ReadFile(c.ConfigPath)
	if err != nil {
		t.Fatal("read config")
	}
	var conf startupConfig
	if err = json.Unmarshal(b, &conf); err != nil {
		t.Fatal("decode config")
	}
	conf.Profile = profile
	if b, err = json.Marshal(conf); err != nil || os.WriteFile(c.ConfigPath, b, 0600) != nil {
		t.Fatal("write config")
	}
}

func routeServer(t *testing.T, profile string) (*Server, string, Config) {
	t.Helper()
	c := testConfig(t)
	setProfile(t, c, profile)
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, c, nil)
	if err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := s.Wait(); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	token, err := os.ReadFile(s.TokenPath())
	if err != nil {
		t.Fatal("token")
	}
	return s, string(token), c
}

func call(t *testing.T, s *Server, token, method, path, key string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.URL()+path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal("request failed")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), token) {
		t.Fatal("response leaked credential")
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestSessionRoutesCreateListSnapshot(t *testing.T) {
	s, token, c := routeServer(t, "memory")
	body := map[string]any{"workspace": c.Workspace}
	status, created := call(t, s, token, "POST", "/v1/sessions", "k1", body)
	if status != 201 {
		t.Fatalf("create status=%d %v", status, created)
	}
	sid, _ := created["sessionId"].(string)
	if sid == "" || created["cursor"] == "" || created["durableSeq"] == nil {
		t.Fatalf("snapshot DTO incomplete: %v", created)
	}
	for _, private := range []string{"FrozenExecutions", "Operations", "Approvals", "stateRoot", "Principal"} {
		if _, ok := created[private]; ok {
			t.Fatalf("private field %s exposed", private)
		}
	}
	status, again := call(t, s, token, "POST", "/v1/sessions", "k1", body)
	if status != 200 || again["sessionId"] != sid {
		t.Fatalf("duplicate status=%d", status)
	}
	if status, _ = call(t, s, token, "POST", "/v1/sessions", "k1", map[string]any{"workspace": c.Workspace, "model": "other"}); status != 409 {
		t.Fatalf("changed request status=%d", status)
	}
	if status, _ = call(t, s, token, "POST", "/v1/sessions", "", body); status != 400 {
		t.Fatalf("missing key status=%d", status)
	}
	if status, _ = call(t, s, token, "POST", "/v1/sessions", "k2", map[string]any{"workspace": c.Workspace, "principal": "evil"}); status != 400 {
		t.Fatalf("unknown field status=%d", status)
	}
	if status, _ = call(t, s, token, "POST", "/v1/sessions", "k3", map[string]any{"workspace": t.TempDir()}); status != 403 {
		t.Fatalf("foreign workspace status=%d", status)
	}
	journal := filepath.Join(s.SessionOptions().StateRoot, "sessions", sid, "journal.jsonl")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal("journal missing")
	}
	status, list := call(t, s, token, "GET", "/v1/sessions", "", nil)
	items, _ := list["sessions"].([]any)
	if status != 200 || len(items) != 1 {
		t.Fatalf("list status=%d %v", status, list)
	}
	if status, snap := call(t, s, token, "GET", "/v1/sessions/"+sid+"/snapshot", "", nil); status != 200 || snap["sessionId"] != sid {
		t.Fatalf("snapshot status=%d", status)
	}
	for _, bad := range []string{"missing", "..%2Fescape"} {
		if status, _ = call(t, s, token, "GET", "/v1/sessions/"+bad+"/snapshot", "", nil); status != 404 {
			t.Fatalf("bad sid %s status=%d", bad, status)
		}
	}
	after, _ := os.ReadFile(journal)
	if !bytes.Equal(before, after) {
		t.Fatal("browsing changed the journal")
	}
}

func TestSessionRoutesDefaultProfileCreatesNothing(t *testing.T) {
	s, token, c := routeServer(t, "")
	if status, _ := call(t, s, token, "POST", "/v1/sessions", "k", map[string]any{"workspace": c.Workspace}); status != 503 {
		t.Fatalf("default profile status=%d", status)
	}
	if _, err := os.Stat(filepath.Join(s.SessionOptions().StateRoot, "sessions")); !os.IsNotExist(err) {
		t.Fatal("default profile wrote a session")
	}
}

func TestBranchRoutes(t *testing.T) {
	s, token, c, _ := offlineServer(t)
	sid := createSession(t, s, token, c)
	_, receipt := call(t, s, token, "POST", "/v1/sessions/"+sid+"/inputs", "i", map[string]any{"kind": "prompt", "content": []map[string]string{{"type": "text", "text": "one"}}})
	tid, _ := receipt["traceId"].(string)
	pollSnapshot(t, s, token, sid, func(m map[string]any) bool { return traceState(m, tid) == "completed" })
	status, page := call(t, s, token, "GET", "/v1/sessions/"+sid+"/messages?limit=1", "", nil)
	msgs, _ := page["messages"].([]any)
	if status != 200 || len(msgs) != 1 {
		t.Fatalf("messages status=%d %v", status, page)
	}
	first, _ := msgs[0].(map[string]any)
	if status, _ = call(t, s, token, "POST", "/v1/sessions/"+sid+"/branches", "", map[string]any{"branchId": "side", "fromEntryId": first["messageId"]}); status != 200 {
		t.Fatalf("fork status=%d", status)
	}
	status, list := call(t, s, token, "GET", "/v1/sessions/"+sid+"/branches", "", nil)
	branches, _ := list["branches"].([]any)
	if status != 200 || len(branches) != 2 {
		t.Fatalf("branches=%v", list)
	}
	if status, _ = call(t, s, token, "POST", "/v1/sessions/"+sid+"/branches/main/activate", "", map[string]any{}); status != 200 {
		t.Fatalf("activate status=%d", status)
	}
	if status, _ = call(t, s, token, "POST", "/v1/sessions/"+sid+"/branches/none/activate", "", map[string]any{}); status != 404 {
		t.Fatalf("missing branch status=%d", status)
	}
	// Strict DTO: summarize is accepted, unknown fields are not. Navigating
	// back to side abandons nothing unique on main, so no model call is made.
	if status, _ = call(t, s, token, "POST", "/v1/sessions/"+sid+"/branches/side/activate", "", map[string]any{"summarize": true, "extra": 1}); status != 400 {
		t.Fatalf("unknown activate field status=%d", status)
	}
	if status, _ = call(t, s, token, "POST", "/v1/sessions/"+sid+"/branches", "", map[string]any{"branchId": "other", "fromEntryId": first["messageId"], "summarize": "yes"}); status != 400 {
		t.Fatalf("non-boolean summarize status=%d", status)
	}
	if status, _ = call(t, s, token, "GET", "/v1/sessions/"+sid+"/messages?after=bogus", "", nil); status != 400 {
		t.Fatalf("bad message cursor status=%d", status)
	}
}

func TestBranchRouteSummarizesAbandonedSuffix(t *testing.T) {
	branchSummary := strings.Join(agent.BranchSummaryHeadings, "\nnone\n") + "\nnone"
	s, token, c, model := offlineServer(t, testkit.Step{Text: "reply"}, testkit.Step{Text: branchSummary})
	sid := createSession(t, s, token, c)
	_, receipt := call(t, s, token, "POST", "/v1/sessions/"+sid+"/inputs", "i", map[string]any{"kind": "prompt", "content": []map[string]string{{"type": "text", "text": "one"}}})
	tid, _ := receipt["traceId"].(string)
	pollSnapshot(t, s, token, sid, func(m map[string]any) bool { return traceState(m, tid) == "completed" })
	_, page := call(t, s, token, "GET", "/v1/sessions/"+sid+"/messages", "", nil)
	msgs, _ := page["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if status, _ := call(t, s, token, "POST", "/v1/sessions/"+sid+"/branches", "", map[string]any{"branchId": "side", "fromEntryId": first["messageId"], "summarize": true}); status != 200 || model.Calls() != 2 {
		t.Fatalf("summarized fork status=%d calls=%d", status, model.Calls())
	}
	_, page = call(t, s, token, "GET", "/v1/sessions/"+sid+"/messages", "", nil)
	msgs, _ = page["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if len(msgs) != 2 || last["kind"] != string(agent.KindBranchSummary) {
		t.Fatalf("summary not on the new path: %v", page)
	}
}

func TestCursorRoundTripAndRejection(t *testing.T) {
	c := EncodeCursor("s1", 42)
	if seq, err := DecodeCursor("s1", c); err != nil || seq != 42 {
		t.Fatal("round trip failed")
	}
	for _, bad := range []string{"!!", c + "A", EncodeCursor("s2", 1), strings.Repeat("A", 600)} {
		if _, err := DecodeCursor("s1", bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
