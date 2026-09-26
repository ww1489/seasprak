package sessions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func TestP2AttemptRetryRemainingRejectsExpiredReservation(t *testing.T) {
	clock := newManualActivityClock()
	lease := &activityLease{clock: clock, limit: time.Minute, committed: true, sample: clock.Now().Add(-time.Second), record: state.ActivityBudget{Reserved: time.Second}}
	if got := lease.remaining(); got != 0 {
		t.Fatalf("expired lease still offered retry time: %v", got)
	}
}

type p2RetryTimeSink struct{ starts, ends int }

func (s *p2RetryTimeSink) CommitFact(_ context.Context, _ agent.ExecutionScope, f agent.Fact) error {
	if f.Kind == "model_attempt_started" {
		s.starts++
	}
	if f.Kind == "assistant" {
		s.ends++
		var body struct {
			AttemptID string `json:"attemptId"`
		}
		if err := json.Unmarshal(f.Payload, &body); err != nil {
			return err
		}
	}
	return nil
}

// Use the native non-streaming runner inside synctest: Eino's streaming event
// copies have GC finalizers outside the virtual-time bubble. Real Chat session
// streaming and durable outcomes are tested separately by the factory suite.
func TestP2AttemptChatRetryAfterAndBackoffCancellation(t *testing.T) {
	for _, scenario := range []string{"retry_after", "cancel", "activity_limit"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var requests atomic.Int32
				var first, second time.Time
				m := p2FactoryModel(t, func(r *http.Request) (*http.Response, error) {
					status := 200
					body := `{"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`
					if requests.Add(1) == 1 {
						first = time.Now()
						status = 429
						body = `{"error":{"message":"synthetic-private-rate-error"}}`
					} else {
						second = time.Now()
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"1"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				sink := &p2RetryTimeSink{}
				budget := agent.NewBudget(config.DefaultLimits())
				remaining := func() time.Duration { return time.Minute }
				if scenario == "activity_limit" {
					remaining = func() time.Duration { return 500 * time.Millisecond }
				}
				ag, err := einorun.NewAgent(ctx, einorun.Deps{Model: m, Sink: sink, Budget: budget, RemainingActivity: remaining})
				if err != nil {
					t.Fatal(err)
				}
				runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag})
				done := make(chan struct{})
				var failed bool
				go func() {
					defer close(done)
					events := runner.Query(ctx, "hello")
					for {
						ev, ok := events.Next()
						if !ok {
							return
						}
						if ev.Err != nil {
							failed = true
						}
					}
				}()
				synctest.Wait()
				if requests.Load() != 1 {
					t.Fatal("retry began before native wait completed")
				}
				if scenario == "cancel" {
					cancel()
				}
				<-done
				want := int32(1)
				if scenario == "retry_after" {
					want = 2
					if second.Sub(first) != time.Second {
						t.Fatalf("native Retry-After wait=%v", second.Sub(first))
					}
				}
				if requests.Load() != want || sink.starts != int(want) || sink.ends != int(want) || budget.Snapshot().TransportRequests != int(want) || failed != (scenario != "retry_after") {
					t.Fatalf("requests=%d starts=%d ends=%d failed=%t", requests.Load(), sink.starts, sink.ends, failed)
				}
			})
		})
	}
}
