package llm_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// This diagnostic emits fixed classifications only, never response text,
// arbitrary header values, request URLs, model names or credentials.
type liveHTTPFailureShape struct {
	JSON, HTML, PlainForbidden                       bool
	InvalidKey, Permission, Quota, RateLimit         bool
	ProxyDenied, NetworkPolicy, WAF                  bool
	CloudflareServer, EnvoyServer, ProxyAuthenticate bool
	BodyBytes                                        int
}

func liveHTTPFailureClassification(raw []byte, headers http.Header) liveHTTPFailureShape {
	lower := strings.ToLower(string(raw))
	var decoded any
	if json.Unmarshal(raw, &decoded) == nil {
		var collect func(any)
		collect = func(value any) {
			switch v := value.(type) {
			case string:
				lower += "\n" + strings.ToLower(v)
			case map[string]any:
				for _, item := range v {
					collect(item)
				}
			case []any:
				for _, item := range v {
					collect(item)
				}
			}
		}
		collect(decoded)
	}
	s := liveHTTPFailureShape{BodyBytes: len(raw), HTML: strings.Contains(lower, "<html") || strings.Contains(lower, "<!doctype html"), PlainForbidden: strings.EqualFold(strings.TrimSpace(string(raw)), "forbidden"), CloudflareServer: strings.EqualFold(headers.Get("Server"), "cloudflare"), EnvoyServer: strings.Contains(strings.ToLower(headers.Get("Server")), "envoy"), ProxyAuthenticate: headers.Get("Proxy-Authenticate") != ""}
	var body struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	s.JSON = json.Valid(raw)
	_ = json.Unmarshal(raw, &body)
	for _, code := range []string{body.Error.Code, body.Error.Type} {
		switch code {
		case "invalid_api_key", "authentication_error":
			s.InvalidKey = true
		case "permission_denied", "permission_error", "forbidden":
			s.Permission = true
		case "insufficient_quota", "quota_exceeded":
			s.Quota = true
		case "rate_limit_exceeded", "rate_limit_error":
			s.RateLimit = true
		}
	}
	s.Permission = s.Permission || strings.Contains(lower, "permission denied") || strings.Contains(lower, "not authorized") || strings.Contains(lower, "无权") || strings.Contains(lower, "没有权限")
	s.ProxyDenied = strings.Contains(lower, "proxy") && (strings.Contains(lower, "denied") || strings.Contains(lower, "forbidden") || strings.Contains(lower, "blocked"))
	s.NetworkPolicy = strings.Contains(lower, "network policy") || strings.Contains(lower, "domain is not allowed") || strings.Contains(lower, "blocked by policy") || strings.Contains(lower, "network access denied") || strings.Contains(lower, "egress")
	s.WAF = strings.Contains(lower, "mod_security") || strings.Contains(lower, "modsecurity") || strings.Contains(lower, "cloudflare") && strings.Contains(lower, "challenge")
	return s
}

type liveHTTPDiagnosticBody struct {
	io.ReadCloser
	t         *testing.T
	protocol  string
	status    int
	headers   http.Header
	collected []byte
	overflow  bool
}

func (b *liveHTTPDiagnosticBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && !b.overflow {
		if len(b.collected)+n > 16384 {
			b.overflow = true
			b.collected = nil
		} else {
			b.collected = append(b.collected, p[:n]...)
		}
	}
	return n, err
}
func (b *liveHTTPDiagnosticBody) Close() error {
	err := b.ReadCloser.Close()
	b.t.Logf("protocol=%s http_status=%d body_overflow=%t failure_shape=%+v", b.protocol, b.status, b.overflow, liveHTTPFailureClassification(b.collected, b.headers))
	b.collected = nil
	return err
}

func TestLiveHTTPDiagnosticBodyPreservesResponseAndClearsBuffer(t *testing.T) {
	for _, size := range []int{23, 16384, 16385} {
		raw := bytes.Repeat([]byte("x"), size)
		body := &liveHTTPDiagnosticBody{ReadCloser: io.NopCloser(bytes.NewReader(raw)), t: t, protocol: "fixture", status: http.StatusForbidden}
		got, err := io.ReadAll(body)
		if err != nil || !bytes.Equal(got, raw) || body.overflow != (size > 16384) {
			t.Fatal("diagnostic changed response or exceeded its collection bound")
		}
		if err := body.Close(); err != nil || body.collected != nil {
			t.Fatal("diagnostic retained private response after close")
		}
	}
}

func TestLiveHTTPFailureClassificationDecodesEscapedMessages(t *testing.T) {
	for _, tc := range []struct {
		raw                 string
		permission, network bool
	}{
		{`{"error":{"message":"\u006e\u006f\u0074 authorized synthetic-private-value"}}`, true, false},
		{`{"message":"\u006e\u0065\u0074work access denied synthetic-private-value"}`, false, true},
		{`{"error":"\u65e0\u6743访问 synthetic-private-value"}`, true, false},
	} {
		s := liveHTTPFailureClassification([]byte(tc.raw), nil)
		if !s.JSON || s.Permission != tc.permission || s.NetworkPolicy != tc.network {
			t.Fatal("decoded fixed classification mismatch")
		}
		raw, _ := json.Marshal(s)
		if bytes.Contains(raw, []byte("synthetic-private-value")) {
			t.Fatal("diagnostic leaked private content")
		}
	}
}

func TestLiveHTTPFailureDiagnosticNeverRetainsPrivateContent(t *testing.T) {
	for _, tc := range []struct {
		raw                    string
		key, permission, proxy bool
	}{
		{`{"error":{"code":"invalid_api_key","message":"synthetic-private-value"}}`, true, false, false},
		{`{"error":{"type":"permission_error","message":"synthetic-private-value"}}`, false, true, false},
		{`<html>Proxy request forbidden synthetic-private-value</html>`, false, false, true},
		{`{"error":{"code":"synthetic-private-value","type":"synthetic-private-value"}}`, false, false, false},
	} {
		s := liveHTTPFailureClassification([]byte(tc.raw), http.Header{"Server": []string{"synthetic-private-value"}})
		if s.InvalidKey != tc.key || s.Permission != tc.permission || s.ProxyDenied != tc.proxy {
			t.Fatal("fixed classification mismatch")
		}
		raw, _ := json.Marshal(s)
		if bytes.Contains(raw, []byte("synthetic-private-value")) {
			t.Fatal("diagnostic leaked private content")
		}
	}
}
