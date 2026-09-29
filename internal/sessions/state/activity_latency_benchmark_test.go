package state

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// Synthetic history isolates size-dependent work without reading session data.
// The store uses the same Chain Prepare/Apply path as the memory backend, without
// importing a concrete backend into state. Each benchmark names its actual work;
// these independent timings are not an instrumented split of a live commit.
type activityBenchmarkStore struct {
	store.Store
	chain *store.Chain
}

func (s *activityBenchmarkStore) Append(_ context.Context, _ string, expected store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	sealed, receipt, duplicate, err := s.chain.Prepare(expected, c)
	if err != nil || duplicate {
		return receipt, err
	}
	return receipt, s.chain.Apply(sealed)
}

func activityBenchmarkView(pages int) *View {
	v := emptyView()
	v.Traces["trace"] = &TraceState{ID: "trace", State: "running", Started: true, Limits: config.DefaultLimits(), Activity: ActivityBudget{Known: true}}
	payload, _ := json.Marshal(strings.Repeat("界", 50*1024/3))
	for i := 0; i < pages; i++ {
		seq := uint64(i + 1)
		v.Events = append(v.Events, agent.Event{Type: "diagnostic", DurableSeq: &seq, Payload: append(json.RawMessage(nil), payload...)})
	}
	v.Cursor = uint64(pages)
	return v
}

func BenchmarkActivityLedgerStages(b *testing.B) {
	for _, pages := range []int{1, 8, 32, 128} {
		b.Run(fmt.Sprintf("pages_%d", pages), func(b *testing.B) {
			v := activityBenchmarkView(pages)
			raw, err := json.Marshal(v)
			if err != nil {
				b.Fatal(err)
			}
			b.Logf("synthetic_view_bytes=%d history_pages=%d page_bytes=51200", len(raw), pages)
			next := *v.Traces["trace"]
			next.Activity = ActivityBudget{Known: true, Reserved: time.Second, ExecutionID: "execution", Revision: 1}
			c := store.Commit{RecordType: "commit", Version: 1, CommitID: "reservation", CommitSeq: 1, ControlRecords: []store.Record{record("trace", "trace", next)}}
			b.Run("EventsAfterCaughtUp", func(b *testing.B) {
				m := &Manager{view: v}
				b.ReportAllocs()
				defer b.ReportMetric(float64(len(raw)), "view-bytes")
				for b.Loop() {
					_, _ = m.EventsAfter(v.Cursor)
				}
			})
			b.Run("EventsAfterOne", func(b *testing.B) {
				m := &Manager{view: v}
				b.ReportAllocs()
				defer b.ReportMetric(float64(len(raw)), "view-bytes")
				for b.Loop() {
					_, _ = m.EventsAfter(v.Cursor - 1)
				}
			})
			b.Run("View", func(b *testing.B) {
				m := &Manager{view: v}
				b.ReportAllocs()
				defer b.ReportMetric(float64(len(raw)), "view-bytes")
				for b.Loop() {
					_ = m.View()
				}
			})
			b.Run("candidate_clone", func(b *testing.B) {
				b.ReportAllocs()
				defer b.ReportMetric(float64(len(raw)), "view-bytes")
				for b.Loop() {
					_ = clone(*v)
				}
			})
			b.Run("apply_reservation", func(b *testing.B) {
				candidate := clone(*v)
				b.ReportAllocs()
				defer b.ReportMetric(float64(len(raw)), "view-bytes")
				for b.Loop() {
					if err := applyCommit(&candidate, c); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("memory_chain_append", func(b *testing.B) {
				s := &activityBenchmarkStore{chain: store.NewChain()}
				seq := uint64(0)
				b.ReportAllocs()
				defer b.ReportMetric(float64(len(raw)), "view-bytes")
				for b.Loop() {
					seq++
					commit := c
					commit.CommitID = fmt.Sprintf("reservation-%d", seq)
					commit.CommitSeq, commit.ExpectedPreviousSeq = seq, seq-1
					if _, err := s.Append(context.Background(), "session", store.ExpectedCommit{ExpectedPreviousSeq: seq - 1}, commit); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("ReserveActivity_with_View_reader", func(b *testing.B) {
				m := &Manager{view: clone(v), store: &activityBenchmarkStore{chain: store.NewChain()}, sessionID: "session"}
				ready, stop := make(chan struct{}), make(chan struct{})
				done := make(chan uint64, 1)
				go func() {
					_ = m.View()
					close(ready)
					reads := uint64(1)
					for {
						select {
						case <-stop:
							done <- reads
							return
						default:
							_ = m.View()
							reads++
						}
					}
				}()
				<-ready
				defer func() { close(stop); b.ReportMetric(float64(<-done), "reader-views") }()
				revision := uint64(0)
				b.ReportAllocs()
				defer b.ReportMetric(float64(len(raw)), "view-bytes")
				for b.Loop() {
					a, err := m.ReserveActivity(context.Background(), "trace", "execution", revision, 0)
					if err != nil {
						b.Fatal(err)
					}
					revision = a.Revision
				}
			})
			b.Run("ReserveActivity", func(b *testing.B) {
				m := &Manager{view: clone(v), store: &activityBenchmarkStore{chain: store.NewChain()}, sessionID: "session"}
				revision := uint64(0)
				b.ReportAllocs()
				defer b.ReportMetric(float64(len(raw)), "view-bytes")
				for b.Loop() {
					a, err := m.ReserveActivity(context.Background(), "trace", "execution", revision, 0)
					if err != nil {
						b.Fatal(err)
					}
					revision = a.Revision
				}
			})
		})
	}
}
