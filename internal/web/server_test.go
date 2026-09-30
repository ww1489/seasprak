package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg := Config{Workspace: t.TempDir(), StateRoot: filepath.Join(t.TempDir(), "private"), ConfigPath: filepath.Join(t.TempDir(), "config.json"), Listen: "127.0.0.1:0"}
	model := llm.ModelConfig{Provider: "openai", Protocol: "openai-chat", Model: "offline", Endpoint: "http://127.0.0.1:1/v1", Version: "v1", AccountScope: "local-test", NoCredentials: true, Parameters: llm.ModelParameters{PolicyVersion: "v1", ConservativeContextWindow: 8192, MaxOutputTokens: 1024}, Capabilities: llm.ModelCapabilities{MaxOutputTokens: 1024, Items: map[llm.CapabilityName]llm.Capability{}}}
	for _, name := range []llm.CapabilityName{llm.CapText, llm.CapOutputLimit, llm.CapPhysicalRequestMetering} {
		model.Capabilities.Items[name] = llm.Capability{Status: llm.Declared, Evidence: []string{"offline-test"}}
	}
	b, err := json.Marshal(startupConfig{Model: model, GenerationFingerprint: "web-test-v1"})
	if err != nil {
		t.Fatal("marshal config")
	}
	if err = os.WriteFile(cfg.ConfigPath, b, 0600); err != nil {
		t.Fatal("write config")
	}
	return cfg
}

func startTestServer(t *testing.T, handler http.Handler) (*Server, context.CancelFunc, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, testConfig(t), handler)
	if err != nil {
		cancel()
		t.Fatalf("start failed: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := s.Wait(); err != nil {
			t.Errorf("shutdown failed: %v", err)
		}
	})
	token, err := os.ReadFile(s.TokenPath())
	if err != nil || len(token) != 43 {
		t.Fatal("credential file unavailable or invalid")
	}
	if err := checkPrivate(s.TokenPath()); err != nil {
		t.Fatal("credential is not protected")
	}
	return s, cancel, string(token)
}

// A connected socket that never sends a request (browser preconnect or a
// transport's spare dial) must not hold shutdown until its deadline.
func TestShutdownClosesUnusedConnections(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, testConfig(t), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", strings.TrimPrefix(s.URL(), "http://"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer conn.Close()
	time.Sleep(50 * time.Millisecond) // let the server register the new connection
	started := time.Now()
	cancel()
	if err := s.Wait(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("shutdown waited %v for an unused connection", elapsed)
	}
}

func TestServerSecurityAndShutdown(t *testing.T) {
	var calls atomic.Int32
	s, cancel, token := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Principal(r.Context()) != "local" {
			t.Error("missing stable principal")
		}
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	client := &http.Client{Timeout: 3 * time.Second}
	for _, tc := range []struct {
		name, host, origin, auth string
		status                   int
	}{
		{"missing", "", "", "", 401}, {"wrong", "", "", "Bearer wrong", 401},
		{"rebind", "attacker.invalid", "", "Bearer " + token, 403},
		{"cross-origin", "", "http://attacker.invalid", "Bearer " + token, 403},
		{"null-origin", "", "null", "Bearer " + token, 403},
		{"origin-path", "", s.URL() + "/", "Bearer " + token, 403},
		{"valid", "", s.URL(), "Bearer " + token, 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, s.URL()+"/future", nil)
			if tc.host != "" {
				req.Host = tc.host
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			before := calls.Load()
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal("request failed")
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status {
				t.Errorf("status=%d want=%d", resp.StatusCode, tc.status)
			}
			if strings.Contains(string(b), token) {
				t.Error("response leaked credential")
			}
			if resp.Header.Get("Access-Control-Allow-Origin") != "" {
				t.Error("CORS enabled")
			}
			expected := before
			if tc.status == 204 {
				expected++
			}
			if calls.Load() != expected {
				t.Error("unauthorized handler invocation")
			}
		})
	}
	path, addr := s.TokenPath(), strings.TrimPrefix(s.URL(), "http://")
	cancel()
	if err := s.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("credential retained after shutdown")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal("listener not released")
	}
	ln.Close()
}

func TestServerUnknownRoutesAndRotation(t *testing.T) {
	first, stop, token := startTestServer(t, nil)
	req, _ := http.NewRequest(http.MethodGet, first.URL()+"/v1/unknown", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unknown route=%d", resp.StatusCode)
	}
	stop()
	if err := first.Wait(); err != nil {
		t.Fatal("stop failed")
	}
	_, _, other := startTestServer(t, nil)
	if token == other {
		t.Error("credential was reused")
	}
}

func TestStartupRejectsInvalidInputs(t *testing.T) {
	for _, name := range []string{"workspace", "state-root", "config", "relative", "nonloopback", "hostname", "overlap", "bad-config", "forbidden-config", "missing-credential", "occupied", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			c := testConfig(t)
			ctx := context.Background()
			switch name {
			case "workspace":
				c.Workspace = ""
			case "state-root":
				c.StateRoot = ""
			case "config":
				c.ConfigPath = ""
			case "relative":
				c.Workspace = "."
			case "nonloopback":
				c.Listen = "0.0.0.0:0"
			case "hostname":
				c.Listen = "localhost:0"
			case "overlap":
				c.StateRoot = filepath.Join(c.Workspace, "state")
			case "bad-config":
				if err := os.WriteFile(c.ConfigPath, []byte(`{"model":{},"unknown":true}`), 0600); err != nil {
					t.Fatal("write")
				}
			case "forbidden-config":
				b, err := os.ReadFile(c.ConfigPath)
				if err != nil {
					t.Fatal("read test config")
				}
				c.ConfigPath = filepath.Join(filepath.Dir(c.ConfigPath), ".test_env")
				if err := os.WriteFile(c.ConfigPath, b, 0600); err != nil {
					t.Fatal("write forbidden config fixture")
				}
			case "missing-credential":
				b, _ := os.ReadFile(c.ConfigPath)
				var v startupConfig
				json.Unmarshal(b, &v)
				v.Model.NoCredentials = false
				v.Model.CredentialRef = "env:SEASPRAK_WEB_MISSING_TEST_CREDENTIAL"
				t.Setenv("SEASPRAK_WEB_MISSING_TEST_CREDENTIAL", "")
				b, _ = json.Marshal(v)
				os.WriteFile(c.ConfigPath, b, 0600)
			case "occupied":
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal("listen")
				}
				defer ln.Close()
				c.Listen = ln.Addr().String()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			s, err := Start(ctx, c, nil)
			if err == nil {
				if s != nil {
					s.Close()
					s.Wait()
				}
				t.Fatal("invalid startup accepted")
			}
			if name == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Error("wrong cancellation error")
				}
			} else {
				want := product.CodeInvalidArgument
				if name == "occupied" || name == "missing-credential" {
					want = product.CodeResourceUnavailable
				}
				var pe *product.Error
				if !errors.As(err, &pe) || pe.Code != want {
					t.Error("wrong startup error code")
				}
			}
			files, _ := filepath.Glob(filepath.Join(c.StateRoot, "web-*.token"))
			if len(files) > 0 {
				t.Error("failed startup left credentials")
			}
		})
	}
}

func TestStrictJSONAndSafeErrors(t *testing.T) {
	type input struct {
		Content string `json:"content"`
	}
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"valid", `{"content":"<script>text only</script>"}`, true},
		{"unknown", `{"content":"x","principal":"admin"}`, false},
		{"multiple", `{"content":"x"}{}`, false}, {"empty", ``, false}, {"null", `null`, false},
		{"oversize", `{"content":"` + strings.Repeat("x", MaxJSONBytes) + `"}`, false},
		{"trailing-overflow", `{"content":"x"}` + strings.Repeat(" ", MaxJSONBytes), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			var v input
			err := DecodeJSON(httptest.NewRecorder(), r, &v)
			if (err == nil) != tc.ok {
				t.Error("unexpected decode result")
			}
			if !tc.ok {
				var pe *product.Error
				if !errors.As(err, &pe) || pe.Code != product.CodeInvalidArgument {
					t.Error("wrong JSON error code")
				}
			}
			if tc.ok && v.Content != "<script>text only</script>" {
				t.Error("content changed")
			}
		})
	}
	for _, tc := range []struct {
		code   string
		status int
	}{
		{product.CodeInvalidArgument, 400}, {product.CodeUnauthenticated, 401}, {product.CodePermissionDenied, 403}, {product.CodeNotFound, 404},
		{product.CodeStateConflict, 409}, {product.CodeIdempotencyConflict, 409}, {product.CodeIncompatibleVersion, 409}, {product.CodeIncompatibleResume, 409}, {product.CodeReconciliationRequired, 409},
		{product.CodeResyncRequired, 410}, {product.CodeUnsupportedCapability, 422}, {product.CodeBudgetExhausted, 422}, {product.CodeStorageUnavailable, 503}, {product.CodeResourceUnavailable, 503}, {product.CodeInternal, 500}, {"unrecognized", 500},
	} {
		t.Run(tc.code, func(t *testing.T) {
			w := httptest.NewRecorder()
			WriteError(w, &product.Error{Code: tc.code, Message: "private backend diagnostic", Details: map[string]string{"private": "hidden"}})
			if w.Code != tc.status {
				t.Errorf("status %d", w.Code)
			}
			var envelope struct {
				Error product.Error `json:"error"`
			}
			if json.Unmarshal(w.Body.Bytes(), &envelope) != nil {
				t.Fatal("invalid error JSON")
			}
			want := tc.code
			if want == "unrecognized" {
				want = product.CodeInternal
			}
			if envelope.Error.Code != want {
				t.Error("wrong error code")
			}
			if envelope.Error.Details != nil || envelope.Error.Refs != nil {
				t.Error("private error fields exposed")
			}
			if strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "hidden") {
				t.Error("private error metadata leaked")
			}
		})
	}
}
