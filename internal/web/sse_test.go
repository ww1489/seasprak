package web

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/testkit"
)

type sseFrame struct{ event, id, data string }

func readFrames(t *testing.T, s *Server, token, path, lastID string, until func([]sseFrame) bool) ([]sseFrame, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.URL()+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal("stream request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, resp.StatusCode
	}
	var frames []sseFrame
	var cur sseFrame
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.event != "" {
				frames = append(frames, cur)
				if until(frames) {
					return frames, 200
				}
			}
			cur = sseFrame{}
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	return frames, 200
}

func TestEventsReplayResumeAndReject(t *testing.T) {
	s, token, c, _ := offlineServer(t, testkit.Step{Text: "answer"})
	sid := createSession(t, s, token, c)
	// The creating catalog keeps the writer, so its stream continues live.
	frames, status := readFrames(t, s, token, "/v1/sessions/"+sid+"/events", "", func(f []sseFrame) bool { return len(f) >= 2 })
	if status != 200 || frames[0].event != "ready" || !strings.Contains(frames[0].data, `"live":true`) {
		t.Fatalf("writer stream: %d %+v", status, frames)
	}
	_, receipt := call(t, s, token, "POST", "/v1/sessions/"+sid+"/inputs", "i", map[string]any{"kind": "prompt", "content": []map[string]string{{"type": "text", "text": "hi"}}})
	tid, _ := receipt["traceId"].(string)
	pollSnapshot(t, s, token, sid, func(m map[string]any) bool { return traceState(m, tid) == "completed" })
	all, _ := readFrames(t, s, token, "/v1/sessions/"+sid+"/events", "", func(f []sseFrame) bool {
		return strings.Contains(f[len(f)-1].data, `"settled":true`)
	})
	var ids []string
	for _, f := range all {
		if f.id != "" {
			ids = append(ids, f.id)
		}
		if strings.Contains(f.data, "principal") || strings.Contains(f.data, "generation") {
			t.Fatalf("private field in event: %s", f.data)
		}
	}
	if len(ids) < 3 {
		t.Fatalf("too few durable frames: %d", len(ids))
	}
	// Resuming from a mid cursor replays exactly the suffix, no duplicates.
	resumed, _ := readFrames(t, s, token, "/v1/sessions/"+sid+"/events", ids[1], func(f []sseFrame) bool {
		n := 0
		for _, x := range f {
			if x.id != "" {
				n++
			}
		}
		return n == len(ids)-2
	})
	var got []string
	for _, f := range resumed {
		if f.id != "" {
			got = append(got, f.id)
		}
	}
	if strings.Join(got, ",") != strings.Join(ids[2:], ",") {
		t.Fatalf("resume mismatch\n got %v\nwant %v", got, ids[2:])
	}
	for _, bad := range []string{"?cursor=garbage", "?cursor=" + EncodeCursor("other", 1), "?cursor=" + EncodeCursor(sid, 1<<40)} {
		if _, status = readFrames(t, s, token, "/v1/sessions/"+sid+"/events"+bad, "", func([]sseFrame) bool { return true }); status == 200 {
			t.Fatalf("bad cursor %s accepted", bad)
		}
	}
	if _, status = readFrames(t, s, token, "/v1/sessions/"+sid+"/events?cursor="+ids[0], ids[1], func([]sseFrame) bool { return true }); status != 400 {
		t.Fatalf("conflicting Last-Event-ID status=%d", status)
	}
}

func TestEventsAfterRestartObserveReadOnly(t *testing.T) {
	c := testConfig(t)
	setProfile(t, c, "memory")
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := os.ReadFile(s.TokenPath())
	sid := createSession(t, s, string(token), c)
	cancel()
	if err = s.Wait(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	s, err = Start(ctx, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = s.Wait() }()
	token, _ = os.ReadFile(s.TokenPath())
	journal := filepath.Join(s.SessionOptions().StateRoot, "sessions", sid, "journal.jsonl")
	before, _ := os.ReadFile(journal)
	frames, status := readFrames(t, s, string(token), "/v1/sessions/"+sid+"/events", "", func(f []sseFrame) bool { return f[len(f)-1].event == "end" })
	if status != 200 || !strings.Contains(frames[0].data, `"live":false`) {
		t.Fatalf("observer stream: %d %+v", status, frames)
	}
	after, _ := os.ReadFile(journal)
	if string(before) != string(after) {
		t.Fatal("observing a session wrote its journal")
	}
}
