package codeagent

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
	"github.com/ww1489/seasprak/internal/codeagent/state"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

type activityLatencyEvent struct {
	at       time.Time
	kind     string
	pc       uintptr
	id       uint64
	duration time.Duration
}

// Numeric timestamps only on the execution path. Formatting and view-size
// measurement happen after settlement, outside the lease being measured.
type activityLatencyClock struct {
	mu     sync.Mutex
	events []activityLatencyEvent
	next   uint64
}

func (c *activityLatencyClock) add(e activityLatencyEvent) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}
func (c *activityLatencyClock) Now() time.Time {
	now := time.Now()
	pc, _, _, _ := goruntime.Caller(1)
	c.add(activityLatencyEvent{at: now, kind: "sample", pc: pc})
	return now
}
func (c *activityLatencyClock) AfterFunc(d time.Duration, fn func()) activityTimer {
	pc, _, _, _ := goruntime.Caller(1)
	c.mu.Lock()
	c.next++
	id := c.next
	c.mu.Unlock()
	c.add(activityLatencyEvent{at: time.Now(), kind: "arm", pc: pc, id: id, duration: d})
	return time.AfterFunc(d, func() {
		c.add(activityLatencyEvent{at: time.Now(), kind: "fire", pc: pc, id: id})
		fn()
	})
}

type activityLatencyStore struct {
	store.Store
	clock   *activityLatencyClock
	settled chan struct{}
	once    sync.Once
}

func (s *activityLatencyStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	start := time.Now()
	s.clock.add(activityLatencyEvent{at: start, kind: "append_enter", id: commit.CommitSeq})
	receipt, err := s.Store.Append(ctx, id, expected, commit)
	end := time.Now()
	s.clock.add(activityLatencyEvent{at: end, kind: "append_exit", id: commit.CommitSeq, duration: end.Sub(start)})
	if err == nil {
		for _, event := range commit.Events {
			if event.Type == "trace.settled" {
				s.once.Do(func() { close(s.settled) })
			}
		}
	}
	return receipt, err
}

func TestActivityLatencyObservationCost(t *testing.T) {
	for _, polling := range []bool{false, true} {
		for _, detailed := range []bool{false, true} {
			t.Run(fmt.Sprintf("polling_%t_detailed_%t", polling, detailed), func(t *testing.T) {
				files := fixture.NewMemory()
				for i := 39; i >= 0; i-- {
					files.SeedFile(fmt.Sprintf("root/%02d.go", i), []byte("needle"+strings.Repeat("界", 600)+"\nneedle"))
				}
				m := &sessionDiscoveryModel{fake: testkit.NewFake(discoveryScript("grep", "grep", `{"root":"root","query":"needle","output_mode":"content","head_limit":100}`), testkit.Step{Text: "done"})}
				m.checks = []func([]*schema.AgenticMessage) (string, error){nil, func(in []*schema.AgenticMessage) (string, error) {
					_, err := discoveryModelPage(in, "grep")
					return "", err
				}}
				id := agent.MustID()
				base, err := memory.Open(id, store.Header{})
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				clock := &activityLatencyClock{}
				st := &activityLatencyStore{Store: base, clock: clock, settled: make(chan struct{})}
				var backend store.Store = st
				var leaseClock activityClock = clock
				var timeline *activityTimeline
				if detailed {
					timeline = &activityTimeline{start: start}
					backend = &activityTimelineStore{Store: st, timeline: timeline}
					leaseClock = timeline
				}
				s, err := CreateAgentSession(t.Context(), Options{SessionID: id, Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: m, Store: backend, Tools: tools.NewBuiltinDefinitions(tools.BuiltinOptions{}), Operations: tools.Operations{Files: files, Artifacts: files}, ResourceScheduler: tools.NewResourceScheduler()})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close(context.Background())
				if err := s.rt.do(t.Context(), func(rt *runtime) error { rt.clock = leaseClock; return nil }); err != nil {
					t.Fatal(err)
				}
				input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"discover"}`)})
				if err != nil {
					t.Fatal(err)
				}
				views := 0
				var viewTime time.Duration
				if polling {
					waitResumeCondition(t, func() bool {
						at := time.Now()
						tr := s.rt.manager.View().Traces[input.TraceID]
						viewTime += time.Since(at)
						views++
						return tr != nil && tr.Settled
					})
				} else {
					select {
					case <-st.settled:
					case <-time.After(5 * time.Second):
						t.Fatal("settlement signal missing")
					case <-t.Context().Done():
						t.Fatal(t.Context().Err())
					}
				}
				elapsed := time.Since(start)
				view := s.rt.manager.View()
				tr := view.Traces[input.TraceID]
				raw, err := json.Marshal(view)
				if err != nil {
					t.Fatal(err)
				}
				// Idle mailbox probe separates queue entry from its mandatory
				// publishCommitted/View work, with and without one View in fn.
				for _, readView := range []bool{false, true} {
					queued := time.Now()
					var entered, finished time.Time
					if err := s.rt.do(t.Context(), func(rt *runtime) error {
						entered = time.Now()
						if readView {
							_ = rt.manager.View()
						}
						finished = time.Now()
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					t.Logf("idle_mailbox read_view=%t queue=%s handler=%s publish_and_reply=%s", readView, entered.Sub(queued), finished.Sub(entered), time.Since(finished))
				}
				clock.mu.Lock()
				events := append([]activityLatencyEvent(nil), clock.events...)
				clock.mu.Unlock()
				var appendTotal, appendMax time.Duration
				var firedAt, renewalAt, appendAt, appendedAt time.Time
				for _, e := range events {
					name := ""
					if f := goruntime.FuncForPC(e.pc); f != nil {
						name = f.Name()
					}
					if e.kind == "fire" && strings.HasSuffix(name, "commitActivity") {
						firedAt = e.at
					}
					if e.kind == "sample" && strings.HasSuffix(name, "beginActivity.func2.1") {
						renewalAt = e.at
						appendAt, appendedAt = time.Time{}, time.Time{}
					}
					if !renewalAt.IsZero() {
						if e.kind == "append_enter" && appendAt.IsZero() {
							appendAt = e.at
						}
						if e.kind == "append_exit" && appendedAt.IsZero() {
							appendedAt = e.at
						}
						if e.kind == "sample" && strings.HasSuffix(name, "commitActivity") && !appendedAt.IsZero() {
							t.Logf("renewal dispatch_bound=%s pre_append=%s append=%s post_append=%s total_since_fire=%s", renewalAt.Sub(firedAt), appendAt.Sub(renewalAt), appendedAt.Sub(appendAt), e.at.Sub(appendedAt), e.at.Sub(firedAt))
							renewalAt = time.Time{}
						}
					}
					if e.kind == "append_exit" {
						appendTotal += e.duration
						appendMax = max(appendMax, e.duration)
					}
					if e.kind != "append_enter" && e.kind != "append_exit" {
						name := ""
						if f := goruntime.FuncForPC(e.pc); f != nil {
							name = f.Name()
						}
						t.Logf("lease at=%s kind=%s site=%s timer=%d duration=%s", e.at.Sub(start), e.kind, name, e.id, e.duration)
					}
				}
				t.Logf("elapsed=%s activity=%s revision=%d polls=%d poll_view_time=%s view_bytes=%d messages=%d events=%d append_total=%s append_max=%s model=%d search=%d gomaxprocs=%d", elapsed, tr.Activity.Settled, tr.Activity.Revision, views, viewTime, len(raw), len(view.Messages), len(view.Events), appendTotal, appendMax, m.fake.Calls(), files.Calls("search"), goruntime.GOMAXPROCS(0))
				if tr.State != "completed" || m.fake.Calls() != 2 || files.Calls("search") != 1 {
					if timeline != nil {
						t.Logf("timeline:\n%s", timeline.dump())
					}
					t.Fatalf("state=%s error=%s activity=%+v model=%d search=%d", tr.State, tr.Error, tr.Activity, m.fake.Calls(), files.Calls("search"))
				}
			})
		}
	}
}

func BenchmarkActivityDiagnosticTraceDecode(b *testing.B) {
	raw, err := json.Marshal(state.TraceState{ID: "trace", State: "running", Started: true, Activity: state.ActivityBudget{Known: true, Reserved: time.Second, ExecutionID: "execution", Revision: 2}})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		var tr state.TraceState
		if err := json.Unmarshal(raw, &tr); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(raw)), "trace-bytes")
}

func BenchmarkActivityDiagnosticClock(b *testing.B) {
	b.Run("system", func(b *testing.B) {
		c := systemActivityClock{}
		for b.Loop() {
			_ = c.Now()
		}
	})
	b.Run("numeric", func(b *testing.B) {
		c := &activityLatencyClock{}
		for b.Loop() {
			_ = c.Now()
			c.events = c.events[:0]
		}
	})
	b.Run("formatted", func(b *testing.B) {
		c := &activityTimeline{start: time.Now()}
		for b.Loop() {
			_ = c.Now()
			c.events = c.events[:0]
		}
	})
}
