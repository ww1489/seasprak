package consumer_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/sdk"
)

func TestConsumerCommandLogResult(t *testing.T) {
	calls := 0
	s, err := sdk.CreateAgentSession(t.Context(), sdk.SessionOptions{
		Workspace: t.TempDir(), StateRoot: "memory", Profile: sdk.ProfileMemory,
		SessionID: "consumer-command-log", Model: &fakeModel{}, Store: &memStore{},
		Operations: sdk.Operations{OutputRedactor: func(_ context.Context, text string) (string, error) {
			calls++
			return strings.ReplaceAll(text, "fixture-private-value", "[redacted]"), nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	var result sdk.CommandResult
	result, err = s.ExecuteCommand(t.Context(), sdk.CommandRequest{Command: "echo fixture-private-value"})
	if err != nil || !result.Started || !result.Terminated || result.ExitCode != 0 || strings.TrimSpace(result.Output) != "[redacted]" || result.Truncated || result.OutputIncomplete || result.LogError != "" || result.Artifact != (sdk.ArtifactRef{}) || calls != 1 {
		t.Fatal("SDK shell result lost output contract", err)
	}
	// The existing SDK alias exposes the new fields and the complete reference,
	// without a second public result type or additional authorization inputs.
	result.Truncated = true
	result.LogError = "resource_unavailable: fixture log unavailable"
	result.Artifact = sdk.ArtifactRef{ID: "artifact", SessionID: "consumer-command-log", Environment: "host", Hash: "fixture-hash", Size: 42, Available: true}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded sdk.CommandResult
	if err := json.Unmarshal(body, &decoded); err != nil || decoded != result {
		t.Fatal("SDK result did not round-trip complete log metadata", err)
	}
}
