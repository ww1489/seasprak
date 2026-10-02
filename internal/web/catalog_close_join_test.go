package web

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func exitedCatalogErrorSession(t *testing.T, c *Catalog, cleanupErr error) *codeagent.AgentSession {
	t.Helper()
	opts := c.opts
	opts.SessionID, opts.StateRoot = "joined-close-error", "memory"
	opts.Model = testkit.NewFake()
	binding, _ := json.Marshal(map[string]string{"hostRealRoot": opts.Workspace})
	backend, err := memory.Open(opts.SessionID, store.Header{Workspace: binding})
	if err != nil {
		t.Fatal(err)
	}
	opts.Store = &catalogCloseErrorStore{Store: backend, err: cleanupErr}
	s, err := codeagent.CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	// Guarantee the error session has really exited even if map iteration
	// visits it only after the other session consumes the deadline.
	if err = s.Close(t.Context()); !errors.Is(err, cleanupErr) {
		t.Fatalf("fixture close=%v", err)
	}
	c.writers[opts.SessionID] = s
	return s
}

func TestCatalogCloseJoinsCleanupAndLiveWriterDeadline(t *testing.T) {
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
	created, err := c.Create(t.Context(), CatalogCreateRequest{Workspace: opts.Workspace, ModelRef: DefaultModelRef, IdempotencyKey: "join"})
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
		t.Fatal("live model did not start")
	}
	cleanupErr := errors.New("fixture cleanup failed")
	exitedCatalogErrorSession(t, c, cleanupErr)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	err = c.Close(ctx)
	cancel()
	if !errors.Is(err, cleanupErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("close=%v, want both cleanup error and deadline", err)
	}
	c.mu.Lock()
	pending, live := len(c.writers), c.writers[sid] == s
	c.mu.Unlock()
	if pending != 1 || !live {
		t.Errorf("pending/live writers=%d/%v", pending, live)
	}
	if other, err := store.OpenCreationRegistry(opts.StateRoot); err == nil {
		_ = other.Close()
		t.Error("registry released while live writer has not exited")
	}
	select {
	case <-m.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not signal cancellation")
	}
	closing := make(chan error, 1)
	go func() { closing <- c.Close(context.Background()) }()
	select {
	case err := <-closing:
		t.Fatalf("second Close finished before runner exit: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	select {
	case err := <-closing:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Close did not finish after runner exit")
	}
	if m.Calls() != 1 {
		t.Fatalf("model calls=%d", m.Calls())
	}
	other, err := store.OpenCreationRegistry(opts.StateRoot)
	if err != nil {
		t.Fatal("registry retained after actual exit")
	}
	_ = other.Close()
}

type closeErrorRegistry struct {
	store.CreationRegistry
	err error
}

func (r *closeErrorRegistry) Close() error {
	return errors.Join(r.CreationRegistry.Close(), r.err)
}

func TestCatalogCloseJoinsSessionAndRegistryErrors(t *testing.T) {
	opts, _ := catalogOptions(t)
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	cleanupErr, registryErr := errors.New("fixture cleanup failed"), errors.New("fixture registry close failed")
	exitedCatalogErrorSession(t, c, cleanupErr)
	c.registry = &closeErrorRegistry{CreationRegistry: c.registry, err: registryErr}
	err = c.Close(t.Context())
	if !errors.Is(err, cleanupErr) || !errors.Is(err, registryErr) {
		t.Errorf("close=%v, want session and registry errors", err)
	}
	other, err := store.OpenCreationRegistry(opts.StateRoot)
	if err != nil {
		t.Fatal("registry not released after both real closes")
	}
	_ = other.Close()
}
