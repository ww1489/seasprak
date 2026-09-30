package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"reflect"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
)

// A page is 50 KiB of accepted assistant text, matching the history-size unit
// used by the activity benchmarks, not an assertion about backend pagination.
// Four genuine completed traces distribute that payload across real messages,
// attempts, events and commits. No synthetic paused trace or View is installed.
const snapshotBenchmarkPageBytes = 50 * 1024

type snapshotBenchmarkStore struct {
	store.Store
	store.CheckpointBlobs
	loads, gets, appends atomic.Int64
}

func (s *snapshotBenchmarkStore) Load(ctx context.Context, id string) (store.StoredSession, error) {
	s.loads.Add(1)
	return s.Store.Load(ctx, id)
}
func (s *snapshotBenchmarkStore) Get(ctx context.Context, id string, ref store.BlobRef) ([]byte, error) {
	s.gets.Add(1)
	return s.CheckpointBlobs.Get(ctx, id, ref)
}
func (s *snapshotBenchmarkStore) Append(ctx context.Context, id string, e store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	s.appends.Add(1)
	return s.Store.Append(ctx, id, e, c)
}

type snapshotBenchmarkModel struct {
	calls   atomic.Int64
	block   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	text    string
}

func (*snapshotBenchmarkModel) Configuration() llm.ModelConfig {
	return llm.ModelConfig{Version: "snapshot-benchmark-v1"}
}
func (m *snapshotBenchmarkModel) Generate(ctx context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.AgenticMessage, error) {
	m.calls.Add(1)
	if m.block.Load() {
		m.once.Do(func() { close(m.entered) })
		select {
		case <-m.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, Extra: map[string]any{"seasprak.finish": "tool_calls"}, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "snapshot-work", Name: "work", Arguments: `{}`})}}, nil
	}
	msg := privateReplayFixture()
	msg.ContentBlocks[2].AssistantGenText.Text = m.text
	return msg, nil
}
func (m *snapshotBenchmarkModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

type snapshotBenchmarkFixture struct {
	s           *AgentSession
	backend     *snapshotBenchmarkStore
	model       *snapshotBenchmarkModel
	tools       atomic.Int64
	clock       *manualActivityClock
	trace       string
	releaseOnce sync.Once
}

func snapshotBenchmarkWait(t testing.TB, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("snapshot fixture condition timed out")
		}
		goruntime.Gosched()
	}
}
func (f *snapshotBenchmarkFixture) release() { f.releaseOnce.Do(func() { close(f.model.release) }) }
func newSnapshotBenchmarkFixture(t testing.TB, pages int, mode string) *snapshotBenchmarkFixture {
	t.Helper()
	id := agent.MustID()
	base, err := memory.Open(id, store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	backend := &snapshotBenchmarkStore{Store: base, CheckpointBlobs: base}
	manager, err := state.NewManager(backend, id)
	if err != nil {
		t.Fatal(err)
	}
	f := &snapshotBenchmarkFixture{backend: backend, clock: newManualActivityClock(), model: &snapshotBenchmarkModel{entered: make(chan struct{}), release: make(chan struct{}), text: strings.Repeat("x", pages*snapshotBenchmarkPageBytes/4)}}
	opts := Options{SessionID: id, Workspace: t.TempDir(), Profile: ProfileMemory, Store: backend, Model: f.model, GenerationFingerprint: "snapshot-benchmark-v1", Limits: config.Limits{ActivityBudget: 365 * 24 * time.Hour}, Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) { f.tools.Add(1); return "done", nil }}}}
	if _, err = alignTools(&opts); err != nil {
		t.Fatal(err)
	}
	f.s, err = Start(opts, manager, "snapshot-generation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.release()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := f.s.Close(ctx); err != nil {
			t.Errorf("close snapshot fixture: %v", err)
		}
	})
	if err = f.s.rt.do(context.Background(), func(rt *runtime) error { rt.clock = f.clock; return nil }); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		r, err := f.s.SubmitInput(context.Background(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"snapshot history"}`)})
		if err != nil {
			t.Fatal(err)
		}
		snapshotBenchmarkWait(t, func() bool {
			// ActivityState avoids cloning megabytes of history while waiting.
			_, _, found := manager.ActivityState(r.TraceID)
			if !found {
				return false
			}
			value, err := f.s.rt.call(context.Background(), func(rt *runtime) (any, error) { return rt.active == nil, nil })
			return err == nil && value.(bool)
		})
		if tr := manager.View().Traces[r.TraceID]; tr.State != "completed" {
			t.Fatalf("history trace state=%s", tr.State)
		}
		if mode == "host_commands" {
			shell := "sh"
			if goruntime.GOOS == "windows" {
				shell = "cmd"
			}
			if _, err := exec.LookPath(shell); err != nil {
				t.Fatalf("host shell unavailable: %v", err)
			}
			result, err := f.s.ExecuteCommand(context.Background(), CommandRequest{Command: "echo snapshot", ExcludeFromContext: i%2 == 0})
			if err != nil || result.ExitCode != 0 {
				t.Fatalf("host command failed: result=%+v err=%v", result, err)
			}
		}
	}
	if mode == "paused" || mode == "running" || mode == "renewal" {
		f.model.block.Store(true)
		r, err := f.s.SubmitInput(context.Background(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"blocked execution"}`)})
		if err != nil {
			t.Fatal(err)
		}
		f.trace = r.TraceID
		select {
		case <-f.model.entered:
		case <-time.After(30 * time.Second):
			t.Fatal("model did not enter")
		}
		if mode == "paused" {
			result := make(chan error, 1)
			go func() { _, err := f.s.Pause(context.Background(), r.TraceID); result <- err }()
			snapshotBenchmarkWait(t, func() bool { return len(manager.View().Operations) == 1 })
			if err := f.s.rt.do(context.Background(), func(*runtime) error { return nil }); err != nil {
				t.Fatal(err)
			}
			f.release()
			select {
			case err := <-result:
				if err != nil {
					t.Fatalf("pause: %v trace error=%q", err, manager.View().Traces[r.TraceID].Error)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("pause timed out")
			}
			snap, err := f.s.Snapshot(context.Background())
			if err != nil || !snap.Resume[r.TraceID].CanResume {
				t.Fatalf("real checkpoint not resumable: err=%v eligibility=%+v", err, snap.Resume[r.TraceID])
			}
		}
	}
	return f
}

// Renewal is a paired workload: one real timer-driven lease renewal racing one
// Snapshot, including synchronization. Its ns/op is not pure Snapshot latency.
func (f *snapshotBenchmarkFixture) snapshotWithRenewal(t testing.TB) Snapshot {
	t.Helper()
	_, before, _ := f.s.rt.manager.ActivityState(f.trace)
	done := make(chan struct{})
	go func() { f.clock.advance(500 * time.Millisecond); close(done) }()
	snap, err := f.s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-done
	snapshotBenchmarkWait(t, func() bool {
		_, after, _ := f.s.rt.manager.ActivityState(f.trace)
		return after.Revision > before.Revision
	})
	// Wait for installation, not just the successful ledger append.
	if err := f.s.rt.do(context.Background(), func(*runtime) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return snap
}

func BenchmarkAgentSessionSnapshot(b *testing.B) {
	for _, pages := range []int{1, 8, 32, 128} {
		for _, mode := range []string{"completed", "host_commands", "paused", "running", "renewal"} {
			b.Run(fmt.Sprintf("pages_%d/history_traces_4/%s", pages, mode), func(b *testing.B) {
				f := newSnapshotBenchmarkFixture(b, pages, mode)
				loads, gets, appends := f.backend.loads.Load(), f.backend.gets.Load(), f.backend.appends.Load()
				calls, runs := f.model.calls.Load(), f.tools.Load()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if mode == "renewal" {
						f.snapshotWithRenewal(b)
					} else if _, err := f.s.Snapshot(context.Background()); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				delta := f.backend.appends.Load() - appends
				want := int64(0)
				if mode == "renewal" {
					want = int64(b.N)
				}
				if delta != want || f.model.calls.Load() != calls || f.tools.Load() != runs {
					b.Fatalf("query effects: append=%d want=%d model=%d tool=%d", delta, want, f.model.calls.Load()-calls, f.tools.Load()-runs)
				}
				b.ReportMetric(float64(f.backend.loads.Load()-loads)/float64(b.N), "Load/op")
				b.ReportMetric(float64(f.backend.gets.Load()-gets)/float64(b.N), "blobGet/op")
				b.ReportMetric(float64(delta)/float64(b.N), "Append/op")
				b.ReportMetric(float64(pages*snapshotBenchmarkPageBytes), "history-text-bytes")
			})
		}
	}
}

func TestSnapshotBenchmarkReadOnlyIsolation(t *testing.T) {
	for _, mode := range []string{"completed", "host_commands", "paused", "running", "renewal"} {
		t.Run(mode, func(t *testing.T) {
			f := newSnapshotBenchmarkFixture(t, 1, mode)
			before := f.s.rt.manager.View()
			calls, runs, appends := f.model.calls.Load(), f.tools.Load(), f.backend.appends.Load()
			loads, gets := f.backend.loads.Load(), f.backend.gets.Load()
			wantCalls := int64(4)
			if mode == "paused" || mode == "running" || mode == "renewal" {
				wantCalls++
			}
			if calls != wantCalls || runs != 0 {
				t.Fatalf("fixture model=%d want=%d tools=%d", calls, wantCalls, runs)
			}
			first, err := f.s.Snapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if f.backend.appends.Load() != appends || f.model.calls.Load() != calls || f.tools.Load() != runs {
				t.Fatal("Snapshot invoked model/tool/Append")
			}
			assertNoPrivateReplay(t, "snapshot benchmark", first)
			if len(first.Traces) != len(before.Traces) || len(first.Messages) < 8 {
				t.Fatal("fixture missing real completed traces/messages")
			}
			internal, err := agent.ConvertToLLM(before.Messages)
			if err != nil {
				t.Fatal(err)
			}
			assistants := 0
			want := privateReplayFixture()
			for _, msg := range internal {
				if msg.Role != schema.AgenticRoleTypeAssistant || len(msg.ContentBlocks) != len(want.ContentBlocks) {
					continue
				}
				assistants++
				if msg.ResponseMeta == nil || msg.ContentBlocks[2].AssistantGenText == nil ||
					!reflect.DeepEqual(msg.ContentBlocks[1].Extra, want.ContentBlocks[1].Extra) ||
					!reflect.DeepEqual(msg.ResponseMeta.Extension, want.ResponseMeta.Extension) ||
					!reflect.DeepEqual(msg.ContentBlocks[2].AssistantGenText.Extension, want.ContentBlocks[2].AssistantGenText.Extension) {
					t.Fatal("fixture lost nested block Extra or private extensions")
				}
			}
			if assistants != 4 {
				t.Fatalf("nested private history messages=%d want=4", assistants)
			}
			raw, err := json.Marshal(internal)
			if err != nil || !strings.Contains(string(raw), "synthetic-private-") {
				t.Fatal("fixture lost private replay payload")
			}
			if mode == "host_commands" {
				var expected []string
				for _, ev := range before.Events {
					if ev.Type == "message.finalized" {
						var msg agent.AgentMessage
						if err := json.Unmarshal(ev.Payload, &msg); err != nil {
							t.Fatal(err)
						}
						expected = append(expected, msg.ID)
					}
				}
				var actual []string
				for _, msg := range first.Messages {
					actual = append(actual, msg.ID)
				}
				if !reflect.DeepEqual(actual, expected) || len(before.HostCommands) != 4 {
					t.Fatal("host command finalization order/deduplication lost")
				}
			}
			first.Messages[1].Standard.ContentBlocks[2].AssistantGenText.Text = "mutated"
			for id, tr := range first.Traces {
				tr.State = "mutated"
				delete(first.Traces, id)
				break
			}
			for id := range first.ModelAttempts {
				delete(first.ModelAttempts, id)
				break
			}
			second, err := f.s.Snapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if second.Messages[1].Standard.ContentBlocks[2].AssistantGenText.Text == "mutated" || len(second.Traces) != len(before.Traces) || len(second.ModelAttempts) != len(before.ModelAttempts) {
				t.Fatal("snapshot mutation escaped isolation")
			}
			if !reflect.DeepEqual(before, f.s.rt.manager.View()) || f.backend.appends.Load() != appends || f.model.calls.Load() != calls || f.tools.Load() != runs {
				t.Fatal("read-only snapshots changed committed state or invoked work")
			}
			if mode == "paused" && !second.Resume[f.trace].CanResume {
				t.Fatal("checkpoint is no longer resumable")
			}
			wantReads := int64(0)
			if mode == "paused" {
				wantReads = 2
			}
			if f.backend.loads.Load()-loads != wantReads || f.backend.gets.Load()-gets != wantReads {
				t.Fatalf("two queries: Load=%d blobGet=%d want=%d", f.backend.loads.Load()-loads, f.backend.gets.Load()-gets, wantReads)
			}
			if mode == "renewal" {
				f.snapshotWithRenewal(t)
				_, after, _ := f.s.rt.manager.ActivityState(f.trace)
				old := before.Traces[f.trace].Activity
				if after.Revision != old.Revision+1 || after.Settled != old.Settled+500*time.Millisecond || f.backend.appends.Load() != appends+1 || f.model.calls.Load() != calls || f.tools.Load() != runs {
					t.Fatal("renewal did not account for exactly one independent Append")
				}
				stable := f.backend.appends.Load()
				if _, err := f.s.Snapshot(t.Context()); err != nil {
					t.Fatal(err)
				}
				if f.backend.appends.Load() != stable {
					t.Fatal("post-renewal Snapshot appended")
				}
			}
		})
	}
}
