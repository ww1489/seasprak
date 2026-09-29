package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

// Test-only real-clock instrumentation. Keep the monotonic time.Time intact;
// logging samples relative durations, never input content or credentials.
type activityTimeline struct {
	mu     sync.Mutex
	start  time.Time
	events []string
	next   int
}

func (a *activityTimeline) log(format string, args ...any) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, fmt.Sprintf("%12s %s", now.Sub(a.start), fmt.Sprintf(format, args...)))
}
func (a *activityTimeline) dump() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.events, "\n")
}
func (a *activityTimeline) Now() time.Time {
	now := time.Now()
	pc, _, line, _ := goruntime.Caller(1)
	name := goruntime.FuncForPC(pc).Name()
	a.log("sample %s:%d at=%s", name, line, now.Sub(a.start))
	return now
}
func (a *activityTimeline) AfterFunc(d time.Duration, fn func()) activityTimer {
	pc, _, line, _ := goruntime.Caller(1)
	name := goruntime.FuncForPC(pc).Name()
	a.mu.Lock()
	a.next++
	id := a.next
	a.mu.Unlock()
	a.log("timer %d arm %s:%d delay=%s", id, name, line, d)
	return time.AfterFunc(d, func() { a.log("timer %d fire-enter", id); fn(); a.log("timer %d fire-exit", id) })
}

type activityTimelineStore struct {
	store.Store
	timeline *activityTimeline
}

func (s *activityTimelineStore) Append(ctx context.Context, id string, e store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	rev := uint64(0)
	var settled, reserved time.Duration
	for _, r := range c.ControlRecords {
		if r.Type == "trace" {
			var tr state.TraceState
			if json.Unmarshal(r.Payload, &tr) == nil {
				rev, settled, reserved = tr.Activity.Revision, tr.Activity.Settled, tr.Activity.Reserved
			}
		}
	}
	s.timeline.log("append-enter seq=%d revision=%d settled=%s reserved=%s", c.CommitSeq, rev, settled, reserved)
	receipt, err := s.Store.Append(ctx, id, e, c)
	s.timeline.log("append-exit seq=%d failed=%t", c.CommitSeq, err != nil)
	return receipt, err
}

func TestActivityFileDiscoveryRealClockTimeline(t *testing.T) {
	clock := &activityTimeline{start: time.Now()}
	files := fixture.NewMemory()
	for i := 39; i >= 0; i-- {
		files.SeedFile(fmt.Sprintf("root/%02d.go", i), []byte("needle"+strings.Repeat("界", 600)+"\nneedle"))
	}
	m := &sessionDiscoveryModel{fake: testkit.NewFake(discoveryScript("grep", "grep", `{"root":"root","query":"needle","output_mode":"content","head_limit":100}`), testkit.Step{Text: "done"})}
	m.checks = []func([]*schema.AgenticMessage) (string, error){nil, func(in []*schema.AgenticMessage) (string, error) {
		clock.log("second model entered")
		_, err := discoveryModelPage(in, "grep")
		return "", err
	}}
	id := agent.MustID()
	backend, err := memory.Open(id, store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := CreateAgentSession(t.Context(), Options{SessionID: id, Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: m, Store: &activityTimelineStore{Store: backend, timeline: clock}, Tools: tools.NewBuiltinDefinitions(tools.BuiltinOptions{}), Operations: tools.Operations{Files: files, Artifacts: files}, ResourceScheduler: tools.NewResourceScheduler()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if err := s.rt.do(t.Context(), func(rt *runtime) error { rt.clock = clock; return nil }); err != nil {
		t.Fatal(err)
	}
	input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"discover files"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { tr := s.rt.manager.View().Traces[input.TraceID]; return tr != nil && tr.Settled })
	view := s.rt.manager.View()
	tr := view.Traces[input.TraceID]
	if tr.State != "completed" || m.fake.Calls() != 2 || files.Calls("search") != 1 {
		t.Fatalf("state=%s error=%s activity=%+v model=%d search=%d timeline:\n%s", tr.State, tr.Error, tr.Activity, m.fake.Calls(), files.Calls("search"), clock.dump())
	}
	t.Logf("activity=%+v model=%d search=%d timeline:\n%s", tr.Activity, m.fake.Calls(), files.Calls("search"), clock.dump())
}
