package web

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

type requestCounter struct{ calls atomic.Int32 }

func (o *requestCounter) BeforeRequest(context.Context, llm.TransportRequest) error {
	o.calls.Add(1)
	return nil
}

type unreadableBody struct{ t *testing.T }

func (b unreadableBody) Read([]byte) (int, error) {
	b.t.Error("unauthorized body was read")
	return 0, nil
}
func (unreadableBody) Close() error { return nil }

func TestSecurityRejectsAmbiguousHeadersBeforeExecution(t *testing.T) {
	model := testkit.NewFake(testkit.Step{Text: "accepted"})
	var calls atomic.Int32
	h := authorize("127.0.0.1:8000", "test-only", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = model.Generate(r.Context(), nil)
		w.WriteHeader(204)
	}))
	for _, name := range []string{"duplicate-auth", "duplicate-origin", "query-token", "forwarded-host", "empty-origin"} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://127.0.0.1:8000/", nil)
			r.Body = unreadableBody{t: t}
			r.Header.Set("Authorization", "Bearer test-only")
			switch name {
			case "duplicate-auth":
				r.Header.Add("Authorization", "Bearer test-only")
			case "duplicate-origin":
				r.Header.Add("Origin", "http://127.0.0.1:8000")
				r.Header.Add("Origin", "http://127.0.0.1:8000")
			case "query-token":
				r.Header.Del("Authorization")
				r.URL.RawQuery = "token=test-only"
			case "forwarded-host":
				r.Host = "attacker.invalid"
				r.Header.Set("X-Forwarded-Host", "127.0.0.1:8000")
			case "empty-origin":
				r.Header["Origin"] = []string{""}
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 401 && w.Code != 403 {
				t.Errorf("status %d", w.Code)
			}
			if calls.Load() != 0 || model.Calls() != 0 {
				t.Error("security rejection invoked execution")
			}
		})
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1:8000/", nil)
	r.Header.Set("Authorization", "Bearer test-only")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 || calls.Load() != 1 || model.Calls() != 1 {
		t.Error("authenticated execution not invoked exactly once")
	}
}

func TestCancellationReachesActiveRequest(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	s, cancel, token := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(exited) }))
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		r, _ := http.NewRequest("GET", s.URL(), nil)
		r.Header.Set("Authorization", "Bearer "+token)
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Do(r)
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not enter handler")
	}
	cancel()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("request context was not cancelled")
	}
	if err := s.Wait(); err != nil {
		t.Fatal("shutdown failed")
	}
	<-requestDone
}

func TestTrustedAssemblyUsesCatalogAndSessionOptions(t *testing.T) {
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test","object":"chat.completion","model":"offline","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer provider.Close()
	c := testConfig(t)
	b, err := os.ReadFile(c.ConfigPath)
	if err != nil {
		t.Fatal("config read failed")
	}
	var conf startupConfig
	if json.Unmarshal(b, &conf) != nil {
		t.Fatal("config parse failed")
	}
	conf.Model.Endpoint = provider.URL + "/v1"
	b, err = json.Marshal(conf)
	if err != nil {
		t.Fatal("config encode failed")
	}
	if os.WriteFile(c.ConfigPath, b, 0600) != nil {
		t.Fatal("config write failed")
	}
	s, err := Start(context.Background(), c, nil)
	if err != nil {
		t.Fatal("start failed")
	}
	defer func() {
		s.Close()
		if s.Wait() != nil {
			t.Error("stop failed")
		}
	}()
	options := s.SessionOptions()
	if options.Model == nil || options.Principal != "local" || options.GenerationFingerprint != "web-test-v1" || options.Workspace != c.Workspace || options.StateRoot != c.StateRoot {
		t.Fatal("session options were not assembled")
	}
	if requests.Load() != 0 {
		t.Fatal("startup contacted model")
	}
	if !llm.UsesObservedTransport(options.Model) {
		t.Fatal("catalog did not install observed factory")
	}
	input := []*schema.AgenticMessage{schema.UserAgenticMessage("test")}
	// The production model must retain the execution-layer metering gate.
	if _, err := options.Model.Generate(context.Background(), input); err == nil || requests.Load() != 0 {
		t.Fatal("model bypassed required request observation")
	}
	observer := &requestCounter{}
	ctx := llm.WithRequestObservation(context.Background(), llm.RequestIdentity{ModelCallID: "test-call", AttemptID: "test-attempt", Purpose: "agent"}, observer)
	msg, err := options.Model.Generate(ctx, input)
	if err != nil || msg == nil || requests.Load() != 1 || observer.calls.Load() != 1 {
		t.Fatal("assembled model did not use real catalog factory exactly once")
	}
}

func TestEnvironmentCredentialScopeAndRotation(t *testing.T) {
	name := "SEASPRAK_WEB_TEST_CREDENTIAL"
	conf := llm.ModelConfig{CredentialRef: "env:" + name, Provider: "openai", Endpoint: "http://127.0.0.1:1/v1", AccountScope: "stable-account"}
	resolver := environmentCredential{model: conf}
	for i := 0; i < 2; i++ {
		secret := rand.Text()
		t.Setenv(name, secret)
		credential, err := resolver.Resolve(context.Background(), conf.CredentialRef)
		if err != nil || credential.Secret != secret || credential.AccountScope != conf.AccountScope || credential.Provider != conf.Provider || credential.Endpoint != conf.Endpoint {
			t.Fatal("credential scope or rotation mismatch")
		}
	}
	if _, err := resolver.Resolve(context.Background(), "env:OTHER"); err == nil {
		t.Error("different credential reference accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolver.Resolve(ctx, conf.CredentialRef); err != context.Canceled {
		t.Error("credential resolver ignored cancellation")
	}
}

func TestDefaultAddressAndPrivateStateRootReuse(t *testing.T) {
	addr, err := listenAddress("")
	if err != nil {
		t.Fatal("default rejected")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		t.Fatal("default is not loopback")
	}
	c := testConfig(t)
	for i := 0; i < 2; i++ {
		s, err := Start(context.Background(), c, nil)
		if err != nil {
			t.Fatal("private state-root reuse failed")
		}
		if strings.HasPrefix(s.TokenPath(), c.Workspace) {
			t.Fatal("token inside workspace")
		}
		s.Close()
		if s.Wait() != nil {
			t.Fatal("shutdown failed")
		}
	}
}
