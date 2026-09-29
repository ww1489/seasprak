package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

type geminiCacheRequest struct {
	path string
	body map[string]any
}
type geminiCacheFixture struct {
	mu            sync.Mutex
	requests      []geminiCacheRequest
	createStatus  int
	cacheName     string
	expiry        time.Time
	rejectCached  bool
	createEntered chan struct{}
	createRelease chan struct{}
	catalog       *llm.Catalog
	config        llm.ModelConfig
	occupied      atomic.Int32
	purposes      []string
}

func newGeminiCacheFixture(t *testing.T) *geminiCacheFixture {
	t.Helper()
	f := &geminiCacheFixture{cacheName: "cachedContents/fixture", expiry: time.Now().Add(time.Hour)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		f.mu.Lock()
		f.requests = append(f.requests, geminiCacheRequest{r.URL.Path, body})
		status, name, expiry, reject := f.createStatus, f.cacheName, f.expiry, f.rejectCached
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/cachedContents") {
			if f.createEntered != nil {
				f.createEntered <- struct{}{}
				select {
				case <-f.createRelease:
				case <-r.Context().Done():
					return
				}
			}
			if status != 0 {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, fmt.Sprintf(`{"error":{"code":%d,"message":"synthetic cache failure"}}`, status))
				return
			}
			response := map[string]any{"name": name}
			if !expiry.IsZero() {
				response["expireTime"] = expiry.Format(time.RFC3339Nano)
			}
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		if reject && body["cachedContent"] != nil {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"cached content missing"}}`)
			return
		}
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, geminiStreamBody("STOP", "answer", false, true))
		} else {
			_, _ = io.WriteString(w, geminiBody("STOP", "answer", false, true))
		}
	}))
	t.Cleanup(server.Close)
	f.config = geminiConfig()
	f.config.Endpoint = server.URL
	f.config.Capabilities.Items[llm.CapCacheResource] = llm.Capability{Status: llm.Declared, Evidence: []string{"synthetic explicit cache fixture"}}
	f.catalog = llm.NewCatalog(p2Resolver(func(context.Context, string) (llm.ResolvedCredential, error) {
		return llm.ResolvedCredential{Secret: "synthetic", AccountScope: f.config.AccountScope, Provider: f.config.Provider, Endpoint: f.config.Endpoint}, nil
	}))
	p2OK(t, f.catalog.RegisterGeminiGenerateContent(server.Client(), 1<<20))
	p2OK(t, f.catalog.Register(f.config))
	return f
}
func (f *geminiCacheFixture) bind(t *testing.T, requested llm.RequestedOptions) llm.Model {
	t.Helper()
	m, err := f.catalog.Bind(f.config.Key(), requested)
	p2OK(t, err)
	return m
}
func (f *geminiCacheFixture) ctx(ctx context.Context, scope string) context.Context {
	return llm.WithRequestObservation(llm.WithSessionCacheScope(ctx, scope), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(_ context.Context, r llm.TransportRequest) error {
		f.occupied.Add(1)
		f.mu.Lock()
		f.purposes = append(f.purposes, r.Purpose)
		f.mu.Unlock()
		return nil
	}))
}
func (f *geminiCacheFixture) snapshot() []geminiCacheRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]geminiCacheRequest(nil), f.requests...)
}
func geminiCacheInput() []*schema.AgenticMessage {
	return []*schema.AgenticMessage{schema.SystemAgenticMessage("stable system"), schema.UserAgenticMessage("question")}
}
func geminiCacheTools(name string) []model.Option {
	return []model.Option{model.WithTools([]*schema.ToolInfo{{Name: name, Desc: "fixture tool", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"q": {Type: schema.String}})}})}
}
func invokeGeminiCache(ctx context.Context, m llm.Model, stream bool, in []*schema.AgenticMessage, opts ...model.Option) error {
	if !stream {
		_, err := m.Generate(ctx, in, opts...)
		return err
	}
	r, err := m.Stream(ctx, in, opts...)
	if err != nil {
		return err
	}
	defer r.Close()
	for {
		_, err = r.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
func TestGeminiExplicitCacheReuseAndEquivalentPayload(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			f := newGeminiCacheFixture(t)
			m := f.bind(t, llm.RequestedOptions{ExplicitCacheResource: true})
			in := geminiCacheInput()
			opts := append(geminiCacheTools("lookup"), model.WithAgenticToolChoice(&schema.AgenticToolChoice{Type: schema.ToolChoiceAllowed}))
			for round := 0; round < 2; round++ {
				p2OK(t, invokeGeminiCache(f.ctx(t.Context(), "session"), m, stream, in, opts...))
			}
			got := f.snapshot()
			if len(got) != 3 || f.occupied.Load() != 3 {
				t.Fatalf("requests=%d occupied=%d", len(got), f.occupied.Load())
			}
			create := got[0].body
			if create["systemInstruction"] == nil || create["tools"] == nil || create["ttl"] != nil || create["expireTime"] != nil {
				t.Fatal("cache must cover system/tools and use provider default retention")
			}
			if c, ok := create["contents"].([]any); ok && len(c) > 0 {
				t.Fatal("system must not be duplicated as cached conversation")
			}
			for _, r := range got[1:] {
				if r.body["cachedContent"] != "cachedContents/fixture" || r.body["systemInstruction"] != nil || r.body["tools"] != nil {
					t.Fatal("cache suffix contains wrong resource or duplicated prefix")
				}
			}
			// Disable active caching to obtain the authoritative full request and compare
			// its semantic prefix/suffix with the real create+generate HTTP bodies.
			full := f.bind(t, llm.RequestedOptions{CacheIntent: "none"})
			p2OK(t, invokeGeminiCache(f.ctx(t.Context(), "session"), full, stream, in, opts...))
			complete := f.snapshot()[3].body
			if !reflect.DeepEqual(create["systemInstruction"], complete["systemInstruction"]) || !reflect.DeepEqual(create["tools"], complete["tools"]) || !reflect.DeepEqual(create["toolConfig"], complete["toolConfig"]) || !reflect.DeepEqual(got[1].body["contents"], complete["contents"]) {
				t.Fatal("cache prefix and suffix are not equivalent to complete request")
			}
			if len(in) != 2 || in[0].Role != schema.AgenticRoleTypeSystem {
				t.Fatal("input mutated")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.purposes[0] != "cache_create" || f.purposes[1] != "agent" {
				t.Fatalf("purposes=%v", f.purposes)
			}
		})
	}
}
func TestGeminiExplicitCacheFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"create_failure", "missing_expiry", "expired", "empty_name", "invalid_name", "missing_resource"} {
			t.Run(fmt.Sprintf("%t/%s", stream, kind), func(t *testing.T) {
				f := newGeminiCacheFixture(t)
				switch kind {
				case "create_failure":
					f.createStatus = 503
				case "missing_expiry":
					f.expiry = time.Time{}
				case "expired":
					f.expiry = time.Now().Add(-time.Hour)
				case "empty_name":
					f.cacheName = ""
				case "invalid_name":
					f.cacheName = "https://other.invalid/cache"
				case "missing_resource":
					f.rejectCached = true
				}
				m := f.bind(t, llm.RequestedOptions{ExplicitCacheResource: true})
				p2OK(t, invokeGeminiCache(f.ctx(t.Context(), "session"), m, stream, geminiCacheInput(), geminiCacheTools("lookup")...))
				got := f.snapshot()
				want := 2
				if kind == "missing_resource" {
					want = 3
				}
				if len(got) != want {
					t.Fatalf("physical=%d want=%d", len(got), want)
				}
				full := got[len(got)-1].body
				if full["cachedContent"] != nil || full["systemInstruction"] == nil || full["tools"] == nil || full["contents"] == nil {
					t.Fatal("fallback lost original complete input")
				}
			})
		}
	}
}
func TestGeminiExplicitCacheOptionsAndScope(t *testing.T) {
	f := newGeminiCacheFixture(t)
	m := f.bind(t, llm.RequestedOptions{ExplicitCacheResource: true, CacheIntent: "long"})
	eff := m.(interface{ EffectiveOptions() llm.EffectiveOptions }).EffectiveOptions()
	if !eff.ActiveCache || eff.CacheIntent != "short" || eff.CacheReason != "provider_default_retention_long_not_guaranteed" {
		t.Fatalf("effective=%+v", eff)
	}
	err := invokeGeminiCache(llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { return nil })), m, false, geminiCacheInput())
	p2Code(t, err, product.CodeInvalidArgument)
	if len(f.snapshot()) != 0 {
		t.Fatal("missing trusted scope reached provider")
	}
	none := f.bind(t, llm.RequestedOptions{ExplicitCacheResource: true, CacheIntent: "none"})
	p2OK(t, invokeGeminiCache(f.ctx(t.Context(), "session"), none, false, geminiCacheInput()))
	if len(f.snapshot()) != 1 || strings.HasSuffix(f.snapshot()[0].path, "/cachedContents") {
		t.Fatal("none created resource")
	}
}
func TestGeminiExplicitCacheBudgetAndCancellation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			f := newGeminiCacheFixture(t)
			m := f.bind(t, llm.RequestedOptions{ExplicitCacheResource: true})
			ctx := llm.WithRequestObservation(llm.WithSessionCacheScope(t.Context(), "session"), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error {
				return product.NewError(product.CodeBudgetExhausted, "denied")
			}))
			err := invokeGeminiCache(ctx, m, stream, geminiCacheInput())
			p2Code(t, err, product.CodeBudgetExhausted)
			ctx, cancel := context.WithCancel(f.ctx(t.Context(), "session"))
			cancel()
			err = invokeGeminiCache(ctx, m, stream, geminiCacheInput())
			if err == nil {
				t.Fatal("cancel ignored")
			}
			if len(f.snapshot()) != 0 {
				t.Fatal("budget/cancellation denial reached provider")
			}
		})
	}
}
func TestGeminiExplicitCacheScopeAndPrefixIsolation(t *testing.T) {
	f := newGeminiCacheFixture(t)
	m := f.bind(t, llm.RequestedOptions{ExplicitCacheResource: true})
	for _, tc := range []struct{ scope, system, tool string }{{"a", "one", "lookup"}, {"a", "one", "lookup"}, {"b", "one", "lookup"}, {"a", "two", "lookup"}, {"a", "one", "changed"}} {
		in := geminiCacheInput()
		in[0] = schema.SystemAgenticMessage(tc.system)
		p2OK(t, invokeGeminiCache(f.ctx(t.Context(), tc.scope), m, false, in, geminiCacheTools(tc.tool)...))
	}
	if len(f.snapshot()) != 9 {
		t.Fatalf("expected 4 creates + 5 generations, got %d", len(f.snapshot()))
	}
}
func TestGeminiExplicitCacheConcurrentCreate(t *testing.T) {
	f := newGeminiCacheFixture(t)
	f.createEntered = make(chan struct{}, 10)
	f.createRelease = make(chan struct{})
	m := f.bind(t, llm.RequestedOptions{ExplicitCacheResource: true})
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- invokeGeminiCache(f.ctx(t.Context(), "session"), m, false, geminiCacheInput())
		}()
	}
	<-f.createEntered
	close(f.createRelease)
	wg.Wait()
	close(errs)
	for err := range errs {
		p2OK(t, err)
	}
	if len(f.snapshot()) != n+1 {
		t.Fatalf("concurrent physical=%d want=%d", len(f.snapshot()), n+1)
	}
}

func TestGeminiExplicitCacheInFlightCancellation(t *testing.T) {
	f := newGeminiCacheFixture(t)
	f.createEntered = make(chan struct{}, 1)
	f.createRelease = make(chan struct{})
	defer close(f.createRelease)
	m := f.bind(t, llm.RequestedOptions{ExplicitCacheResource: true})
	ctx, cancel := context.WithCancel(f.ctx(t.Context(), "session"))
	done := make(chan error, 1)
	go func() { done <- invokeGeminiCache(ctx, m, false, geminiCacheInput()) }()
	<-f.createEntered
	cancel()
	if err := <-done; err == nil {
		t.Fatal("in-flight cache creation ignored cancellation")
	}
	if len(f.snapshot()) != 1 || f.occupied.Load() != 1 {
		t.Fatal("canceled cache creation initiated fallback generation")
	}
}

func TestGeminiExplicitCacheFallbackCannotBypassBudget(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			f := newGeminiCacheFixture(t)
			f.createStatus = 503
			m := f.bind(t, llm.RequestedOptions{ExplicitCacheResource: true})
			var occupied atomic.Int32
			ctx := llm.WithRequestObservation(llm.WithSessionCacheScope(t.Context(), "session"), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(_ context.Context, r llm.TransportRequest) error {
				if occupied.Add(1) > 1 {
					return product.NewError(product.CodeBudgetExhausted, "denied")
				}
				if r.TransportAttempt != 1 || r.Purpose != "cache_create" {
					t.Error("wrong auxiliary budget identity")
				}
				return nil
			}))
			err := invokeGeminiCache(ctx, m, stream, geminiCacheInput())
			p2Code(t, err, product.CodeBudgetExhausted)
			if len(f.snapshot()) != 1 || occupied.Load() != 2 {
				t.Fatal("full fallback bypassed shared physical budget")
			}
		})
	}
}

func TestGeminiExplicitCachePrefixSelection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         []*schema.AgenticMessage
		tools      bool
		wantCreate bool
	}{
		{"no_prefix", []*schema.AgenticMessage{schema.UserAgenticMessage("question")}, false, false},
		{"tools_only", []*schema.AgenticMessage{schema.UserAgenticMessage("question")}, true, true},
		{"system_without_suffix", []*schema.AgenticMessage{schema.SystemAgenticMessage("system")}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGeminiCacheFixture(t)
			m := f.bind(t, llm.RequestedOptions{ExplicitCacheResource: true})
			var opts []model.Option
			if tc.tools {
				opts = geminiCacheTools("lookup")
			}
			p2OK(t, invokeGeminiCache(f.ctx(t.Context(), "session"), m, false, tc.in, opts...))
			want := 1
			if tc.wantCreate {
				want = 2
			}
			if len(f.snapshot()) != want {
				t.Fatal("incorrect prefix selected for explicit resource")
			}
		})
	}
}

func TestGeminiExplicitCacheCreationAuthorizationFailure(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newGeminiCacheFixture(t)
			f.createStatus = status
			m := f.bind(t, llm.RequestedOptions{ExplicitCacheResource: true})
			err := invokeGeminiCache(f.ctx(t.Context(), "session"), m, false, geminiCacheInput())
			p2Code(t, err, product.CodeResourceUnavailable)
			wantKind := "authentication"
			if status == 403 {
				wantKind = "permission"
			}
			info, ok := llm.ModelFailure(err)
			if !ok || info.Kind != wantKind {
				t.Fatal("trusted authorization classification lost")
			}
			if len(f.snapshot()) != 1 || f.occupied.Load() != 1 {
				t.Fatal("authorization failure initiated another request")
			}
		})
	}
}

func TestGeminiExplicitCacheRequiresResourceCapability(t *testing.T) {
	f := newGeminiCacheFixture(t)
	delete(f.config.Capabilities.Items, llm.CapCacheResource)
	_, err := llm.ResolveOptions(f.config, llm.RequestedOptions{ExplicitCacheResource: true})
	p2Code(t, err, product.CodeUnsupportedCapability)
	options, err := llm.ResolveOptions(f.config, llm.RequestedOptions{ExplicitCacheResource: true, CacheIntent: "none"})
	p2OK(t, err)
	if options.ActiveCache || options.CacheReason != "active_cache_disabled_implicit_cache_not_controlled" {
		t.Fatal("none did not override resource opt-in")
	}
}

func TestGeminiExplicitCacheToolPairSuffixAndDefinitionChanges(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			f := newGeminiCacheFixture(t)
			m := f.bind(t, llm.RequestedOptions{ExplicitCacheResource: true})
			in := append(geminiCacheInput(), &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "call-id", Name: "lookup", Arguments: `{"q":"value"}`})}}, &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: "call-id", Name: "lookup", Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: "result"}}}})}})
			p2OK(t, invokeGeminiCache(f.ctx(t.Context(), "session"), m, stream, in, geminiCacheTools("lookup")...))
			cached := f.snapshot()[1].body
			full := f.bind(t, llm.RequestedOptions{CacheIntent: "none"})
			p2OK(t, invokeGeminiCache(f.ctx(t.Context(), "session"), full, stream, in, geminiCacheTools("lookup")...))
			if !reflect.DeepEqual(cached["contents"], f.snapshot()[2].body["contents"]) {
				t.Fatal("tool pair suffix changed")
			}
			contents, _ := cached["contents"].([]any)
			if len(contents) != 3 {
				t.Fatal("tool pair was split or dropped")
			}
			changed := model.WithTools([]*schema.ToolInfo{{Name: "lookup", Desc: "changed definition", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"different": {Type: schema.Integer}})}})
			p2OK(t, invokeGeminiCache(f.ctx(t.Context(), "session"), m, stream, in, changed))
			if len(f.snapshot()) != 5 {
				t.Fatal("changed tool schema reused old resource")
			}
			p2OK(t, invokeGeminiCache(f.ctx(t.Context(), "session"), m, stream, in, changed, model.WithAgenticToolChoice(&schema.AgenticToolChoice{Type: schema.ToolChoiceForbidden})))
			if len(f.snapshot()) != 7 {
				t.Fatal("changed tool policy reused old resource")
			}
		})
	}
}
