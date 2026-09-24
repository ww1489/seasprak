package consumer_test

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/sdk"
)

type fakeModel struct {
	mu    sync.Mutex
	calls int
}

func (m *fakeModel) Generate(ctx context.Context, _ []*schema.AgenticMessage, _ ...einomodel.Option) (*schema.AgenticMessage, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return &schema.AgenticMessage{
		Role:          schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "ok"})},
		Extra:         map[string]any{"seasprak.finish": "stop"},
	}, ctx.Err()
}

func (m *fakeModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil && msg == nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), err
}

type nopSink struct{}

func (nopSink) CommitFact(context.Context, sdk.ExecutionScope, sdk.Fact) error { return nil }

type memStore struct {
	mu      sync.Mutex
	commits []sdk.Commit
}

func (s *memStore) Load(context.Context, string) (sdk.StoredSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sdk.StoredSession{Commits: append([]sdk.Commit(nil), s.commits...)}, nil
}

func (s *memStore) Append(_ context.Context, _ string, _ sdk.ExpectedCommit, commit sdk.Commit) (sdk.CommitReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits = append(s.commits, commit)
	return sdk.CommitReceipt{CommitID: commit.CommitID, CommitSeq: commit.CommitSeq}, nil
}

func (s *memStore) ReadAfter(_ context.Context, _ string, after uint64) (sdk.CommitReader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var items []sdk.Commit
	for _, c := range s.commits {
		if c.CommitSeq > after {
			items = append(items, c)
		}
	}
	return &sliceReader{items: items}, nil
}

func (s *memStore) Close() error { return nil }

func (s *memStore) used() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.commits) > 0
}

type sliceReader struct {
	items []sdk.Commit
	i     int
}

func (r *sliceReader) Next() (sdk.Commit, error) {
	if r.i >= len(r.items) {
		return sdk.Commit{}, io.EOF
	}
	item := r.items[r.i]
	r.i++
	return item, nil
}

func TestConsumerLayers(t *testing.T) {
	var model sdk.Model = &fakeModel{}
	msg, err := model.Generate(context.Background(), nil)
	if err != nil || msg == nil {
		t.Fatalf("model generate: %v", err)
	}
	stream, err := model.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()

	budget := sdk.NewBudget(sdk.DefaultLimits())
	exec, err := sdk.NewExecutor("gen", nil, nopSink{}, nil, budget)
	if err != nil {
		t.Fatal(err)
	}
	if exec == nil {
		t.Fatal("executor")
	}
	ag, err := sdk.NewAgent(context.Background(), sdk.AgentDeps{Model: model, Sink: nopSink{}, Budget: budget, Instruction: "test"})
	if err != nil {
		t.Fatal(err)
	}
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag})
	iter := runner.Query(context.Background(), "ping")
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			t.Fatal(ev.Err)
		}
	}

	store := &memStore{}
	var _ sdk.SessionStore = store
	var _ sdk.ExecutionSink = nopSink{}
	s, err := sdk.CreateAgentSession(context.Background(), sdk.SessionOptions{
		Workspace: t.TempDir(), StateRoot: "memory", Profile: sdk.ProfileMemory,
		SessionID: "consumer-sess", Model: model, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	receipt, err := s.SubmitInput(ctx, sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hi"}`)})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := s.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if tr := snap.Traces[receipt.TraceID]; tr != nil && tr.Settled {
			if tr.State != "completed" {
				t.Fatalf("state=%s err=%s", tr.State, tr.Error)
			}
			if err := s.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if !store.used() {
				t.Fatal("custom store was unused")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("session did not settle")
}
