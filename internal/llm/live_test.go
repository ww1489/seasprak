//go:build live

package llm_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/llm"
)

// Each configuration certifies only the actually exercised protocol factory.
// With maintainer approval, absent protocol configurations are probed against
// the existing OPENAI gateway/model without changing origin or inventing keys.
func TestLocalCompatibleModel(t *testing.T) {
	env, loadErr := loadEnv()
	for _, tc := range []struct {
		name, prefix string
		config       func() llm.ModelConfig
		register     func(*llm.Catalog, *http.Client, int) error
	}{
		{"OpenAIChat", "OPENAI", p2ChatConfig, (*llm.Catalog).RegisterOpenAIChat},
		{"OpenAIResponses", "OPENAI_RESPONSES", p2ResponsesConfig, (*llm.Catalog).RegisterOpenAIResponses},
		{"DeepSeekChat", "DEEPSEEK", p2DeepSeekConfig, (*llm.Catalog).RegisterDeepSeekChat},
		{"AnthropicMessages", "ANTHROPIC", anthropicConfig, (*llm.Catalog).RegisterAnthropicMessages},
		{"GeminiGenerateContent", "GEMINI", geminiConfig, (*llm.Catalog).RegisterGeminiGenerateContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if loadErr != nil {
				t.Skip("缺配置：无法加载本地 live 配置；未验证此工厂")
			}
			connection, err := resolveLiveConnection(env, tc.prefix)
			if err != nil {
				t.Fatal("live配置不完整或地址不安全；不会将其他地址的凭据拼入此配置")
			}
			if connection.derived {
				t.Log("使用已授权的同源网关及原模型派生配置；是否支持此协议由真实请求断言判定")
			}
			if tc.prefix == "GEMINI" {
				u, _ := url.Parse(connection.endpoint)
				t.Logf("gemini_config: dedicated=%t official_host=%t root_path=%t version_path=%t model_path=%t", !connection.derived, u.Hostname() == "generativelanguage.googleapis.com", u.Path == "" || u.Path == "/", u.Path == "/v1beta" || u.Path == "/v1", strings.Contains(u.Path, "/models/"))
			}
			cfg := tc.config()
			cfg.Model = connection.model
			cfg.Endpoint = connection.endpoint
			// Synthetic fixtures use 128 output tokens. A live reasoning model
			// needs a bounded allowance large enough to finish the tool arguments.
			cfg.Parameters.MaxOutputTokens = 2048
			cfg.Capabilities.MaxOutputTokens = 2048
			cfg.NoCredentials = false
			cfg.CredentialRef = "local-live"
			cfg.AccountScope = "local-live"
			// Offline fixture declarations are prerequisites, not verified live capability evidence.
			for name, cap := range cfg.Capabilities.Items {
				if cap.Status == llm.Declared {
					cap.Evidence = []string{"fixed factory offline test declaration; live probe pending"}
					cfg.Capabilities.Items[name] = cap
				}
			}
			catalog := llm.NewCatalog(p2Resolver(func(context.Context, string) (llm.ResolvedCredential, error) {
				return llm.ResolvedCredential{Secret: connection.key, Provider: cfg.Provider, Endpoint: cfg.Endpoint, AccountScope: cfg.AccountScope}, nil
			}))
			var physical atomic.Int32
			client := &http.Client{Timeout: 40 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				physical.Add(1)
				if tc.prefix == "OPENAI" {
					configuredURL, _ := url.Parse(connection.endpoint)
					proxy, proxyErr := http.ProxyFromEnvironment(r)
					versions := 0
					for _, segment := range strings.Split(r.URL.Path, "/") {
						if segment == "v1" {
							versions++
						}
					}
					t.Logf("request_shape: configured_origin=%t chat_path=%t v1_segments=%d credential_matches=%t auth_headers=%d environment_proxy=%t proxy_lookup_failed=%t user_agent_explicit=%t", r.URL.Scheme == configuredURL.Scheme && r.URL.Host == configuredURL.Host, strings.HasSuffix(r.URL.Path, "/v1/chat/completions"), versions, r.Header.Get("Authorization") == "Bearer "+connection.key, len(r.Header.Values("Authorization")), proxy != nil, proxyErr != nil, r.UserAgent() != "")
				}
				response, err := http.DefaultTransport.RoundTrip(r)
				if err != nil && tc.prefix == "GEMINI" {
					var networkError net.Error
					t.Logf("gemini_transport_failure: eof=%t unexpected_eof=%t timeout=%t canceled=%t", errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.As(err, &networkError) && networkError.Timeout(), errors.Is(err, context.Canceled))
				}
				if err == nil {
					t.Logf("protocol=%s http_status=%d", tc.prefix, response.StatusCode)
					if response.StatusCode >= 400 {
						response.Body = &liveHTTPDiagnosticBody{ReadCloser: response.Body, t: t, protocol: tc.prefix, status: response.StatusCode, headers: response.Header.Clone()}
					}
				}
				if err == nil && tc.prefix == "GEMINI" {
					response.Body = &liveGeminiDiagnosticBody{ReadCloser: response.Body, t: t, status: response.StatusCode}
				}
				return response, err
			})}
			acceptanceError(t, "register_factory", tc.register(catalog, client, 1<<20))
			acceptanceError(t, "register_config", catalog.Register(cfg))
			m, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
			acceptanceError(t, "bind", err)
			if physical.Load() != 0 {
				t.Fatal("binding initiated a physical request")
			}
			var expectedRequests int32
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
					expectedRequests += 3
					acceptanceConversation(t, m, cfg, &physical, stream)
				})
			}
			if !t.Failed() && physical.Load() != expectedRequests {
				t.Fatal("factory probe requires exactly three physical requests per selected conversation")
			}
		})
	}
}

func loadEnv() (map[string]string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for {
		path := filepath.Join(dir, ".test_env")
		if raw, err := os.ReadFile(path); err == nil {
			return parse(string(raw)), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, os.ErrNotExist
		}
		dir = parent
	}
}

func parse(text string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		out[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return out
}
