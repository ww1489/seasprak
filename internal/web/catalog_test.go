package web

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func catalogOptions(t *testing.T) (codeagent.Options, *testkit.FakeModel) {
	t.Helper()
	m := testkit.NewFake()
	return codeagent.Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), Principal: "local", Profile: codeagent.ProfileMemory, Model: m, GenerationFingerprint: "catalog-test-v1"}, m
}
func TestCatalogCreateReplayAndReadOnly(t *testing.T) {
	opts, m := catalogOptions(t)
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	req := CatalogCreateRequest{IdempotencyKey: "same", Workspace: opts.Workspace, ModelRef: "default"}
	first, err := c.Create(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Create(t.Context(), req)
	if err != nil || !second.Duplicate || second.Snapshot.SessionID != first.Snapshot.SessionID {
		t.Fatal("duplicate created another session")
	}
	req.ModelRef = "other"
	_, err = c.Create(t.Context(), req)
	if e, ok := product.AsError(err); !ok || e.Code != product.CodeIdempotencyConflict {
		t.Fatal("same key changed request accepted")
	}
	if err = c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	c, err = NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	req.ModelRef = "default"
	reopened, err := c.Create(t.Context(), req)
	if err != nil || !reopened.Duplicate {
		t.Fatal("durable creation was lost")
	}
	want, _ := json.Marshal(first.Snapshot)
	got, _ := json.Marshal(reopened.Snapshot)
	if string(want) != string(got) {
		t.Fatal("original receipt changed")
	}
	journal := filepath.Join(opts.StateRoot, "sessions", first.Snapshot.SessionID, "journal.jsonl")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.Snapshot(t.Context(), first.Snapshot.SessionID)
	if err != nil || snap.SessionID != first.Snapshot.SessionID {
		t.Fatal("snapshot failed")
	}
	list, err := c.List(t.Context(), CatalogListRequest{})
	if err != nil || len(list.Sessions) != 1 || list.Sessions[0].SessionID != snap.SessionID {
		t.Fatal("list failed")
	}
	sub, live, release, err := c.Subscribe(t.Context(), snap.SessionID, 0, config.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if live {
		t.Fatal("history browsing opened a writer")
	}
	for {
		select {
		case _, ok := <-sub.Events:
			if !ok {
				goto ended
			}
		case <-time.After(5 * time.Second):
			t.Fatal("history did not end")
		}
	}
ended:
	after, err := os.ReadFile(journal)
	if err != nil || string(before) != string(after) || m.Calls() != 0 || len(c.writers) != 0 {
		t.Fatal("read-only browsing wrote or executed")
	}
}
func TestCatalogReusesReservationAfterCrash(t *testing.T) {
	opts, m := catalogOptions(t)
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	req := CatalogCreateRequest{IdempotencyKey: "crash", Workspace: opts.Workspace, ModelRef: DefaultModelRef}
	pending := store.CreationRecord{SessionID: "reserved01", Digest: c.digest(req), Generation: opts.GenerationFingerprint}
	if err = c.registry.Save(t.Context(), registryKey(opts.Principal, req.IdempotencyKey), pending); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	c, err = NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	got, err := c.Create(t.Context(), req)
	if err != nil || got.Snapshot.SessionID != "reserved01" || !got.Duplicate || m.Calls() != 0 {
		t.Fatalf("pending reservation not reused: %v %s", err, got.Snapshot.SessionID)
	}
	list, err := c.List(t.Context(), CatalogListRequest{})
	if err != nil || len(list.Sessions) != 1 {
		t.Fatal("reservation created a second session")
	}
}
func TestCatalogConcurrentCreate(t *testing.T) {
	opts, m := catalogOptions(t)
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := c.Create(t.Context(), CatalogCreateRequest{IdempotencyKey: "one", Workspace: opts.Workspace, ModelRef: "default"})
			if e != nil {
				t.Error("concurrent creation failed")
				return
			}
			ids <- r.Snapshot.SessionID
		}()
	}
	wg.Wait()
	close(ids)
	id, n := "", 0
	for got := range ids {
		n++
		if id != "" && got != id {
			t.Error("duplicate sid")
		}
		id = got
	}
	if n != 8 || m.Calls() != 0 {
		t.Fatal("creation counts mismatch")
	}
}
func TestCatalogRejectsForeignWorkspaceAndClosedAccess(t *testing.T) {
	opts, _ := catalogOptions(t)
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Create(t.Context(), CatalogCreateRequest{IdempotencyKey: "x", Workspace: t.TempDir(), ModelRef: "default"})
	if e, ok := product.AsError(err); !ok || e.Code != product.CodePermissionDenied {
		t.Fatal("foreign workspace accepted")
	}
	for _, id := range []string{"../escape", "missing"} {
		if _, err = c.Snapshot(t.Context(), id); err == nil {
			t.Fatal("invalid session read")
		}
	}
	if err = c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = c.List(t.Context(), CatalogListRequest{}); err == nil {
		t.Fatal("closed catalog readable")
	}
}

type catalogUncooperativeModel struct {
	*testkit.FakeModel
	started   chan struct{}
	cancelled chan struct{}
}

func (m *catalogUncooperativeModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	close(m.started)
	go func() { <-ctx.Done(); close(m.cancelled) }()
	return m.FakeModel.Generate(context.Background(), in, opts...)
}
func (m *catalogUncooperativeModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}
func TestCatalogCloseTimeoutRetainsWriterAndRegistryUntilExit(t *testing.T) {
	opts, _ := catalogOptions(t)
	gate := make(chan struct{})
	m := &catalogUncooperativeModel{FakeModel: testkit.NewFake(testkit.Step{Gate: gate, Text: "done"}), started: make(chan struct{}), cancelled: make(chan struct{})}
	opts.Model = m
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
		_ = c.Close(context.Background())
	})
	created, err := c.Create(t.Context(), CatalogCreateRequest{Workspace: opts.Workspace, ModelRef: DefaultModelRef, IdempotencyKey: "create"})
	if err != nil {
		t.Fatal(err)
	}
	sid := created.Snapshot.SessionID
	s, err := c.Writer(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"wait"}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		closing := make(chan error, 1)
		go func() { closing <- c.Close(ctx) }()
		if i == 0 {
			select {
			case <-m.cancelled:
			case <-time.After(5 * time.Second):
				t.Fatal("close did not signal cancellation")
			}
			access := make(chan error, 1)
			go func() { _, err := c.List(t.Context(), CatalogListRequest{}); access <- err }()
			select {
			case err := <-access:
				if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict {
					t.Errorf("closing access error=%v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("closing catalog blocked new business until execution exit")
			}
		}
		err = <-closing
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("close %d error=%v, want actual exit wait timeout", i, err)
		}
		if len(c.writers) != 1 {
			t.Error("live writer ownership released before actual exit")
		}
		if other, e := store.OpenCreationRegistry(opts.StateRoot); e == nil {
			other.Close()
			t.Error("registry ownership released before actual exit")
		}
	}
	if _, err = c.List(t.Context(), CatalogListRequest{}); err == nil {
		t.Error("closing catalog accepted new access")
	}
	close(gate)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err = c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.writers) != 0 || m.Calls() != 1 {
		t.Fatalf("writer/model counts=%d/%d", len(c.writers), m.Calls())
	}
	other, err := store.OpenCreationRegistry(opts.StateRoot)
	if err != nil {
		t.Fatal("registry not released after actual exit")
	}
	other.Close()
	opts.SessionID = sid
	reopened, err := codeagent.OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal("session writer not released after actual exit")
	}
	reopened.Close(t.Context())
}

type catalogCloseErrorStore struct {
	store.Store
	err error
}

func (s *catalogCloseErrorStore) Close() error {
	if err := s.Store.Close(); err != nil {
		return err
	}
	return s.err
}

func TestCatalogCloseErrorPreservesErrorAndReleasesRegistryAfterExit(t *testing.T) {
	opts, _ := catalogOptions(t)
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.registry.Close()
	opts.SessionID, opts.StateRoot = "close-error-session", "memory"
	binding, _ := json.Marshal(map[string]string{"hostRealRoot": opts.Workspace})
	backend, err := memory.Open(opts.SessionID, store.Header{Workspace: binding})
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("fixture store close failed")
	opts.Store = &catalogCloseErrorStore{Store: backend, err: want}
	s, err := codeagent.CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	c.writers[opts.SessionID] = s
	if err = c.Close(t.Context()); !errors.Is(err, want) {
		t.Fatalf("close error=%v", err)
	}
	if len(c.writers) != 0 {
		t.Error("exited writer retained after close error")
	}
	other, err := store.OpenCreationRegistry(c.opts.StateRoot)
	if err != nil {
		t.Fatal("registry retained after actual exit with close error")
	}
	other.Close()
}
