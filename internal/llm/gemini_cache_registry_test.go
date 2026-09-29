package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/llm/einoext/agenticgemini"
	"google.golang.org/genai"
)

type geminiCacheObserver struct{ calls atomic.Int32 }

type geminiCacheWaitingContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *geminiCacheWaitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestGeminiCacheCanceledCreatorDoesNotCancelWaiter(t *testing.T) {
	r := newGeminiCacheRegistry()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	leader := make(chan error, 1)
	go func() {
		_, err := r.acquire(ctx, "same", geminiCacheCoverage{}, func(ctx context.Context) (*genai.CachedContent, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		leader <- err
	}()
	<-entered
	waitCtx := &geminiCacheWaitingContext{Context: t.Context(), waiting: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() {
		e, err := r.acquire(waitCtx, "same", geminiCacheCoverage{}, func(context.Context) (*genai.CachedContent, error) {
			t.Error("waiter silently retried cache creation")
			return nil, nil
		})
		if e != nil {
			t.Error("canceled creation supplied a cache handle")
		}
		waiter <- err
	}()
	<-waitCtx.waiting
	cancel()
	if err := <-leader; err != context.Canceled {
		t.Fatalf("leader error=%v", err)
	}
	if err := <-waiter; err != nil {
		t.Fatalf("independent waiter must use complete fallback: %v", err)
	}
}

func (o *geminiCacheObserver) BeforeRequest(context.Context, TransportRequest) error {
	o.calls.Add(1)
	return nil
}

func TestGeminiCacheRegistryTTLAndBindingHTTP(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	var creates, generations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1beta/cachedContents" {
			n := creates.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": fmt.Sprintf("cachedContents/resource%d", n), "expireTime": now().Add(time.Minute).Format(time.RFC3339Nano)})
			return
		}
		generations.Add(1)
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"answer"}]},"finishReason":"STOP"}]}`)
	}))
	defer server.Close()
	client := server.Client()
	client.Transport = NewObservedTransport(client.Transport)
	attempts := int32(1)
	api, err := genai.NewClient(t.Context(), &genai.ClientConfig{APIKey: "synthetic", Backend: genai.BackendGeminiAPI, HTTPClient: client, HTTPOptions: genai.HTTPOptions{BaseURL: server.URL, RetryOptions: &genai.HTTPRetryOptions{Attempts: &attempts}}})
	if err != nil {
		t.Fatal(err)
	}
	inner, err := agenticgemini.New(t.Context(), &agenticgemini.Config{Client: api, Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	registry := newGeminiCacheRegistry()
	registry.now = now
	base := ResolvedModelConfig{Config: ModelConfig{Provider: "google", Protocol: "gemini-generate-content", Endpoint: server.URL, Model: "fixture", Version: "config"}, AccountScope: "account", Options: EffectiveOptions{Version: "policy"}}
	observer := &geminiCacheObserver{}
	ctx := WithRequestObservation(WithSessionCacheScope(t.Context(), "session"), RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, observer)
	in := []*schema.AgenticMessage{schema.SystemAgenticMessage("prefix"), schema.UserAgenticMessage("question")}
	invoke := func(c ResolvedModelConfig) {
		t.Helper()
		m := &geminiCachedModel{inner: inner, registry: registry, config: c}
		if _, err := m.Generate(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	invoke(base)
	invoke(base)
	if creates.Load() != 1 {
		t.Fatal("factory model lifetime lost cache reuse")
	}
	clock.Add(int64(time.Minute))
	invoke(base)
	if creates.Load() != 2 {
		t.Fatal("expired resource was reused")
	}
	for _, change := range []func(*ResolvedModelConfig){func(c *ResolvedModelConfig) { c.Config.Provider = "other" }, func(c *ResolvedModelConfig) { c.Config.Endpoint = "https://other.invalid" }, func(c *ResolvedModelConfig) { c.AccountScope = "other" }, func(c *ResolvedModelConfig) { c.Config.Model = "other" }, func(c *ResolvedModelConfig) { c.Options.Version = "other" }, func(c *ResolvedModelConfig) { c.Config.Version = "other" }} {
		c := base
		change(&c)
		invoke(c)
	}
	if creates.Load() != 8 || generations.Load() != 9 || observer.calls.Load() != 17 {
		t.Fatalf("creates=%d generations=%d occupied=%d", creates.Load(), generations.Load(), observer.calls.Load())
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, e := range registry.entries {
		if !e.coverage.System || e.coverage.Tools || e.coverage.Messages != 0 {
			t.Fatalf("coverage=%+v", e.coverage)
		}
	}
}
func TestGeminiCacheRegistryBoundAndExpiredCleanup(t *testing.T) {
	r := newGeminiCacheRegistry()
	now := time.Now()
	r.now = func() time.Time { return now }
	creates := 0
	create := func(context.Context) (*genai.CachedContent, error) {
		creates++
		return &genai.CachedContent{Name: "cachedContents/fixture", ExpireTime: now.Add(time.Hour)}, nil
	}
	for i := 0; i < geminiCacheRegistryLimit; i++ {
		e, err := r.acquire(t.Context(), fmt.Sprint(i), geminiCacheCoverage{}, create)
		if err != nil || e == nil {
			t.Fatal("failed to fill registry")
		}
	}
	if e, err := r.acquire(t.Context(), "overflow", geminiCacheCoverage{}, create); err != nil || e != nil {
		t.Fatal("full registry must select complete request")
	}
	if creates != geminiCacheRegistryLimit || len(r.entries) != geminiCacheRegistryLimit {
		t.Fatal("registry exceeded bound")
	}
	now = now.Add(time.Hour)
	if e, err := r.acquire(t.Context(), "fresh", geminiCacheCoverage{}, create); err != nil || e == nil || len(r.entries) != 1 {
		t.Fatal("expired resources were not cleaned")
	}
}
func TestGeminiCacheRegistryCanceledWaiter(t *testing.T) {
	r := newGeminiCacheRegistry()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := r.acquire(t.Context(), "same", geminiCacheCoverage{}, func(context.Context) (*genai.CachedContent, error) {
			close(entered)
			<-release
			return &genai.CachedContent{Name: "cachedContents/fixture", ExpireTime: time.Now().Add(time.Hour)}, nil
		})
		done <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waitCtx := &geminiCacheWaitingContext{Context: ctx, waiting: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() {
		_, err := r.acquire(waitCtx, "same", geminiCacheCoverage{}, func(context.Context) (*genai.CachedContent, error) {
			t.Error("waiter created resource")
			return nil, nil
		})
		waiter <- err
	}()
	<-waitCtx.waiting
	cancel()
	if err := <-waiter; err != context.Canceled {
		t.Fatalf("cancel error=%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
