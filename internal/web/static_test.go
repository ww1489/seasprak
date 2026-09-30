package web

import (
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

func rawGet(t *testing.T, s *Server, path, token string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", s.URL()+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal("request failed")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(body)
}

func TestStaticPageServedBeforeAuthAPIStillProtected(t *testing.T) {
	s, token, _ := routeServer(t, "memory")
	status, h, index := rawGet(t, s, "/", "")
	if status != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/html") || h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("index status=%d headers=%v", status, h)
	}
	if strings.Contains(index, token) {
		t.Fatal("index contains the bearer")
	}
	if status, _, again := rawGet(t, s, "/index.html", ""); status != 200 || again != index {
		t.Fatalf("/index.html status=%d", status)
	}
	m := regexp.MustCompile(`/assets/[A-Za-z0-9_.-]+\.js`).FindString(index)
	if m == "" {
		t.Fatal("index references no hashed script")
	}
	if status, h, _ := rawGet(t, s, m, ""); status != 200 || h.Get("Content-Type") != "text/javascript; charset=utf-8" || !strings.Contains(h.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset status=%d type=%s", status, h.Get("Content-Type"))
	}
	if css := regexp.MustCompile(`/assets/[A-Za-z0-9_.-]+\.css`).FindString(index); css != "" {
		if status, h, _ := rawGet(t, s, css, ""); status != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/css") {
			t.Fatalf("css status=%d type=%s", status, h.Get("Content-Type"))
		}
	}
	for _, bad := range []string{"/assets/../server.go", "/assets/..%2Fserver.go", "/assets/%2e%2e/auth.go", "/assets/", "/assets/missing.js", "/assets/sub/x.js", "/static/index.html", "/server.go", "/favicon.ico"} {
		if status, _, body := rawGet(t, s, bad, ""); status == 200 || strings.Contains(body, "package web") {
			t.Fatalf("%s status=%d", bad, status)
		}
	}
	// The API and every non-static path still require the bearer.
	for _, p := range []string{"/v1/sessions", "/v1/sessions/x/render", "/v1/sessions/x/ui/events"} {
		if status, _, _ := rawGet(t, s, p, ""); status != 401 {
			t.Fatalf("%s without token status=%d", p, status)
		}
	}
	if status, _, _ := rawGet(t, s, "/v1/sessions", token); status != 200 {
		t.Fatalf("authorized list status=%d", status)
	}
	req, _ := http.NewRequest("POST", s.URL()+"/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal("post failed")
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("POST / without token status=%d", resp.StatusCode)
	}
	// Host and Origin checks still apply to the static page.
	req, _ = http.NewRequest("GET", s.URL()+"/", nil)
	req.Host = "evil.example"
	if resp, err = http.DefaultClient.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("foreign Host static status=%v", resp)
	}
	resp.Body.Close()
	req, _ = http.NewRequest("GET", s.URL()+"/", nil)
	req.Header.Set("Origin", "http://evil.example")
	if resp, err = http.DefaultClient.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("foreign Origin static status=%v", resp)
	}
	resp.Body.Close()
}

func TestEmbeddedAssetsContainNoSecretsOrExternalLoads(t *testing.T) {
	// Tokens are random per process, so the embedded files are checked for
	// credential shapes; a live token is checked in the page test above.
	secret := regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}|(?i)api[_-]?key\s*[:=]|web-[A-Za-z0-9]+\.token`)
	files := 0
	err := fs.WalkDir(static, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		b, _ := static.ReadFile(p)
		text := string(b)
		if m := secret.FindString(text); m != "" {
			t.Errorf("%s contains a credential-like value", p)
		}
		for _, bad := range []string{"sourceMappingURL", "localStorage", "EventSource", "fonts.googleapis", "cdn.", "\\Users\\", "/home/"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
		if strings.HasSuffix(p, ".html") && (strings.Contains(text, "http://") || strings.Contains(text, "https://")) {
			t.Errorf("%s loads an absolute URL", p)
		}
		return nil
	})
	if err != nil || files < 2 {
		t.Fatalf("embedded build missing: files=%d err=%v", files, err)
	}
}
