package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type startupOutput struct{ ready chan string }

func (w startupOutput) Write(p []byte) (int, error) { w.ready <- string(p); return len(p), nil }

func TestCommandConfiguredStartupAndCancellation(t *testing.T) {
	workspace := t.TempDir()
	state := filepath.Join(t.TempDir(), "private")
	config := filepath.Join(t.TempDir(), "config.json")
	body := `{"generationFingerprint":"cli-test-v1","model":{"Provider":"openai","Protocol":"openai-chat","Model":"offline","Endpoint":"http://127.0.0.1:1/v1","Version":"v1","AccountScope":"local-test","NoCredentials":true,"Parameters":{"PolicyVersion":"v1","ConservativeContextWindow":8192,"MaxOutputTokens":1024},"Capabilities":{"MaxOutputTokens":1024,"Items":{"text":{"Status":"declared","Evidence":["offline-test"]},"output_limit":{"Status":"declared","Evidence":["offline-test"]},"physical_request_metering":{"Status":"declared","Evidence":["offline-test"]}}}}}`
	if err := os.WriteFile(config, []byte(body), 0600); err != nil {
		t.Fatal("config write failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := startupOutput{ready: make(chan string, 1)}
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--web", "--workspace", workspace, "--state-root", state, "--config", config, "--listen", "127.0.0.1:0"}, output, &stderr)
	}()
	var announcement string
	select {
	case announcement = <-output.ready:
	case code := <-done:
		t.Fatalf("startup exited early: %d", code)
	case <-time.After(5 * time.Second):
		t.Fatal("startup not ready")
	}
	lines := strings.Split(strings.TrimSpace(announcement), "\n")
	if len(lines) != 2 {
		t.Fatal("unexpected startup output")
	}
	url := strings.TrimPrefix(strings.TrimSpace(lines[0]), "Listening: ")
	path := strings.TrimPrefix(strings.TrimSpace(lines[1]), "Bearer file: ")
	token, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("credential path not usable")
	}
	if strings.Contains(announcement, string(token)) {
		t.Fatal("startup output leaked bearer")
	}
	req, err := http.NewRequest("GET", url+"/", nil)
	if err != nil {
		t.Fatal("invalid announcement URL")
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("configured command did not listen")
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("placeholder route status=%d", resp.StatusCode)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("shutdown exit=%d", code)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("command did not stop")
	}
	if stderr.Len() != 0 {
		t.Error("unexpected error output")
	}
	ln, err := net.Listen("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal("command retained listener")
	}
	ln.Close()
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Error("command retained credential")
	}
}
