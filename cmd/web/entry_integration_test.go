package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/llm"
)

func TestWebEntryStartsAndStopsSubprocess(t *testing.T) {
	if os.Getenv("SEASPRAK_WEB_LIFECYCLE_HELPER") == "1" {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			var signal [1]byte
			_, _ = os.Stdin.Read(signal[:])
			cancel()
		}()
		os.Exit(run(ctx, []string{"--web", "--listen", "127.0.0.1:0", "--workspace", os.Getenv("SEASPRAK_WEB_TEST_WORKSPACE"), "--state-root", os.Getenv("SEASPRAK_WEB_TEST_STATE"), "--config", os.Getenv("SEASPRAK_WEB_TEST_CONFIG")}, os.Stdout, os.Stderr))
	}
	workspace := t.TempDir()
	stateRoot := filepath.Join(t.TempDir(), "private")
	configPath := filepath.Join(t.TempDir(), "config.json")
	model := llm.ModelConfig{
		Provider: "openai", Protocol: "openai-chat", Model: "offline", Version: "v1",
		Endpoint: "http://127.0.0.1:1/v1", AccountScope: "entry-test", NoCredentials: true,
		Parameters:   llm.ModelParameters{PolicyVersion: "v1", ConservativeContextWindow: 8192, MaxOutputTokens: 1024},
		Capabilities: llm.ModelCapabilities{MaxOutputTokens: 1024, Items: map[llm.CapabilityName]llm.Capability{}},
	}
	for _, name := range []llm.CapabilityName{llm.CapText, llm.CapOutputLimit, llm.CapPhysicalRequestMetering} {
		model.Capabilities.Items[name] = llm.Capability{Status: llm.Declared, Evidence: []string{"offline fixture"}}
	}
	data, err := json.Marshal(struct {
		Model      llm.ModelConfig `json:"model"`
		Generation string          `json:"generationFingerprint"`
	}{model, "entry-test-v1"})
	if err != nil || os.WriteFile(configPath, data, 0o600) != nil {
		t.Fatal("cannot create startup fixture")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("cannot locate test process")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestWebEntryStartsAndStopsSubprocess$")
	cmd.Env = append(os.Environ(), "SEASPRAK_WEB_LIFECYCLE_HELPER=1", "SEASPRAK_WEB_TEST_WORKSPACE="+workspace, "SEASPRAK_WEB_TEST_STATE="+stateRoot, "SEASPRAK_WEB_TEST_CONFIG="+configPath)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("cannot create shutdown channel")
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("cannot observe startup")
	}
	// Provider errors and credentials must never appear in failure diagnostics.
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		t.Fatal("cannot start entry subprocess")
	}
	waited := false
	defer func() {
		_ = input.Close()
		if !waited {
			cancel()
			_ = cmd.Wait()
		}
	}()
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() || !strings.HasPrefix(scanner.Text(), "Listening: ") {
		t.Fatal("entry did not announce listener")
	}
	url := strings.TrimPrefix(scanner.Text(), "Listening: ")
	if !scanner.Scan() || !strings.HasPrefix(scanner.Text(), "Bearer file: ") {
		t.Fatal("entry did not announce protected credential path")
	}
	tokenPath := strings.TrimPrefix(scanner.Text(), "Bearer file: ")
	token, err := os.ReadFile(tokenPath)
	if err != nil || len(token) != 43 {
		t.Fatal("credential unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/sessions", nil)
	if err != nil {
		t.Fatal("invalid listener URL")
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal("entry listener is unreachable")
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("session list route status=%d", response.StatusCode)
	}
	if _, err = input.Write([]byte{1}); err != nil {
		t.Fatal("cannot request shutdown")
	}
	_ = input.Close()
	err = cmd.Wait()
	waited = true
	if err != nil {
		t.Fatal("entry shutdown failed")
	}
	if _, err = os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatal("entry retained credential after shutdown")
	}
	listener, err := net.Listen("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal("entry retained listener after shutdown")
	}
	_ = listener.Close()
}
