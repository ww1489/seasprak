package llm_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// This helper is test-only. Derived configurations are probes of the same
// supplied gateway/model, not declarations of native-vendor support.
type liveConnection struct {
	model, endpoint, key string
	derived              bool
}

func resolveLiveConnection(env map[string]string, prefix string) (liveConnection, error) {
	var out liveConnection
	source := prefix
	present := false
	for _, suffix := range []string{"MODEL", "BASE_URL", "API_KEY"} {
		if strings.TrimSpace(env[prefix+"_"+suffix]) != "" {
			present = true
		}
	}
	// Never fill credentials into a partially configured destination. A supplied
	// per-protocol connection is authoritative and must be complete on its own.
	if !present && prefix != "OPENAI" {
		source = "OPENAI"
		out.derived = true
	}
	out.model = strings.TrimSpace(env[source+"_MODEL"])
	out.endpoint = strings.TrimRight(strings.TrimSpace(env[source+"_BASE_URL"]), "/")
	out.key = strings.TrimSpace(env[source+"_API_KEY"])
	if out.model == "" || out.endpoint == "" || out.key == "" {
		return liveConnection{}, errors.New("incomplete live connection")
	}
	u, err := url.Parse(out.endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return liveConnection{}, errors.New("invalid live endpoint")
	}
	if prefix == "OPENAI" || out.derived {
		switch prefix {
		case "OPENAI", "OPENAI_RESPONSES", "DEEPSEEK":
			if !strings.HasSuffix(u.Path, "/v1") {
				u.Path += "/v1"
			}
		case "ANTHROPIC", "GEMINI":
			// Their native SDKs add their own v1/messages or v1beta/models route.
			u.Path = strings.TrimSuffix(u.Path, "/v1")
		default:
			return liveConnection{}, errors.New("unknown live protocol prefix")
		}
		out.endpoint = strings.TrimRight(u.String(), "/")
	}
	return out, nil
}

func TestLiveConnectionDerivationKeepsOriginAndCredentials(t *testing.T) {
	for _, base := range []string{"https://fixture.invalid", "https://fixture.invalid/v1", "https://fixture.invalid/gateway/v1"} {
		env := map[string]string{"OPENAI_MODEL": "fixture-model", "OPENAI_BASE_URL": base, "OPENAI_API_KEY": "synthetic-local-only"}
		for _, prefix := range []string{"OPENAI", "OPENAI_RESPONSES", "DEEPSEEK", "ANTHROPIC", "GEMINI"} {
			got, err := resolveLiveConnection(env, prefix)
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(got.endpoint)
			original, _ := url.Parse(base)
			if u.Scheme != original.Scheme || u.Host != original.Host || got.key != env["OPENAI_API_KEY"] || got.model != env["OPENAI_MODEL"] || got.derived != (prefix != "OPENAI") {
				t.Fatal("derived connection changed trusted binding")
			}
			want := strings.TrimSuffix(original.Path, "/v1")
			if prefix == "OPENAI" || prefix == "OPENAI_RESPONSES" || prefix == "DEEPSEEK" {
				want += "/v1"
			}
			if u.Path != want {
				t.Fatal("derived route root mismatch")
			}
		}
		if len(env) != 3 {
			t.Fatal("derivation changed source configuration")
		}
	}
}

func TestLiveConnectionExplicitBindingIsNeverMixed(t *testing.T) {
	env := map[string]string{"OPENAI_MODEL": "fixture-model", "OPENAI_BASE_URL": "https://first.invalid/v1", "OPENAI_API_KEY": "synthetic-first", "GEMINI_BASE_URL": "https://second.invalid"}
	if _, err := resolveLiveConnection(env, "GEMINI"); err == nil {
		t.Fatal("partial foreign endpoint inherited another credential")
	}
	env["GEMINI_MODEL"], env["GEMINI_API_KEY"] = "fixture-gemini", "synthetic-second"
	got, err := resolveLiveConnection(env, "GEMINI")
	if err != nil || got.derived || got.endpoint != env["GEMINI_BASE_URL"] || got.key != env["GEMINI_API_KEY"] || got.model != env["GEMINI_MODEL"] {
		t.Fatal("explicit connection was overwritten")
	}
}

func TestLiveConnectionRejectsUnsafeURLWithoutEcho(t *testing.T) {
	for _, endpoint := range []string{"https://user:synthetic@fixture.invalid/v1", "https://fixture.invalid/v1?key=synthetic", "https://fixture.invalid/v1#synthetic", "file:///synthetic", "https://fixture.invalid/%2Fsynthetic"} {
		_, err := resolveLiveConnection(map[string]string{"OPENAI_MODEL": "fixture", "OPENAI_BASE_URL": endpoint, "OPENAI_API_KEY": "synthetic"}, "GEMINI")
		if err == nil || strings.Contains(err.Error(), "synthetic") {
			t.Fatal("unsafe endpoint accepted or echoed")
		}
	}
}
