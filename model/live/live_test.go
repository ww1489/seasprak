//go:build live

package live

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalCompatibleModel(t *testing.T) {
	env, err := loadEnv()
	if err != nil {
		t.Skip(err)
	}
	body, _ := json.Marshal(map[string]any{
		"model":    env["OPENAI_MODEL"],
		"messages": []map[string]string{{"role": "user", "content": "Reply with the single word pong."}},
	})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(env["OPENAI_BASE_URL"], "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+env["OPENAI_API_KEY"])
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(redact(err.Error(), env["OPENAI_API_KEY"]))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("status %s", resp.Status)
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

func redact(message, secret string) string {
	if secret == "" {
		return message
	}
	return strings.ReplaceAll(message, secret, "[redacted]")
}
