package consumer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/sdk"
)

// This file uses only the public SDK and standard library. The injected-model
// consumers elsewhere intentionally also import Eino's model interface types.
func TestSDKConsumerChatFactoryApprovalReopen(t *testing.T) {
	var requests, runs atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if r.URL.Path != "/chat/completions" || json.NewDecoder(r.Body).Decode(&body) != nil || !body.Stream {
			t.Error("factory did not issue a streaming Chat request")
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch requests.Add(1) {
		case 1:
			fmt.Fprint(w, "data: {\"id\":\"chat-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-work\",\"type\":\"function\",\"function\":{\"name\":\"work\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
		case 2:
			found := false
			for _, message := range body.Messages {
				if message.Role == "tool" && strings.Contains(string(message.Content), "factory-approved-result") {
					found = true
				}
			}
			if !found {
				t.Error("resumed factory did not receive the original tool result")
			}
			fmt.Fprint(w, "data: {\"id\":\"chat-2\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			t.Error("unexpected repeated physical request")
		}
	}))
	defer server.Close()
	catalog := sdk.NewCatalog(nil)
	if err := catalog.RegisterOpenAIChat(server.Client(), 1<<20); err != nil {
		t.Fatal(err)
	}
	config := sdk.ModelConfig{Provider: "fixture", Protocol: "openai-chat", Model: "fixture-model", Endpoint: server.URL, Version: "v1", AccountScope: "fixture-account", NoCredentials: true}
	config.Parameters.PolicyVersion = "v1"
	config.Parameters.MaxOutputTokens = 256
	config.Parameters.ConservativeContextWindow = 8192
	config.Capabilities.MaxOutputTokens = 256
	config.Capabilities.Items = map[sdk.CapabilityName]sdk.Capability{}
	for _, name := range []sdk.CapabilityName{"text", "text_stream", "tools", "physical_request_metering", "output_limit"} {
		config.Capabilities.Items[name] = sdk.Capability{Status: "declared", Evidence: []string{"local httptest fixture"}}
	}
	if err := catalog.Register(config); err != nil {
		t.Fatal(err)
	}
	model, err := catalog.Bind(config.Key(), sdk.RequestedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	def := sdk.ToolDefinition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
		runs.Add(1)
		return "factory-approved-result", nil
	}}
	def.Execution.Effect, def.Execution.RequestedGrantRef = "read", "one-operation"
	opts := sdk.SessionOptions{SessionID: "consumer-factory-approval", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sdk.ProfileMemory, Principal: "host-user", Model: model, Tools: []sdk.ToolDefinition{def}, GenerationFingerprint: "consumer-factory-v1"}
	s, err := sdk.CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	input, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"run work"}`)})
	if err != nil {
		t.Fatal(err)
	}
	before := waitConsumerApprovalState(t, s, input.TraceID, "paused")
	original := onlyConsumerApproval(t, before)
	if requests.Load() != 1 || runs.Load() != 0 {
		t.Fatal("ask did not stop before tool execution")
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Explicit read-only browsing must neither ask nor execute, and rejects Resume.
	browseOpts := opts
	browseOpts.ReadOnly, browseOpts.Model = true, nil
	browse, err := sdk.OpenAgentSession(t.Context(), browseOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = browse.Close(context.Background()) })
	browsed, err := browse.Snapshot(t.Context())
	if err != nil || browsed.Cursor != before.Cursor || browsed.Revision != before.Revision || len(browsed.Interactions) != 0 {
		t.Fatalf("read-only open changed facts: %v", err)
	}
	_, err = browse.Resume(t.Context(), sdk.ResumeCommand{TraceID: input.TraceID, ExpectedRevision: browsed.Revision})
	if pe, ok := sdk.AsError(err); !ok || pe.Code != sdk.CodePermissionDenied {
		t.Fatalf("read-only Resume error: %v", err)
	}
	if err := browse.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	opened, err := sdk.OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	snap, err := opened.Snapshot(t.Context())
	if err != nil || snap.Cursor != before.Cursor || snap.Revision != before.Revision || len(snap.Interactions) != 0 || requests.Load() != 1 || runs.Load() != 0 {
		t.Fatalf("open wrote, asked or executed: %v", err)
	}
	if _, err := opened.Resume(t.Context(), sdk.ResumeCommand{TraceID: input.TraceID, ExpectedRevision: snap.Revision}); err != nil {
		t.Fatal(err)
	}
	snap = waitConsumerApprovalState(t, opened, input.TraceID, "paused")
	fresh := onlyConsumerApproval(t, snap)
	if fresh.ID == original.ID || fresh.ApprovalID == original.ApprovalID || fresh.CallID != original.CallID || fresh.TargetRef != "" || requests.Load() != 1 || runs.Load() != 0 {
		t.Fatal("Resume did not ask afresh for the same private-target-redacted call")
	}
	receipt, err := opened.RespondInteraction(t.Context(), sdk.InteractionResponse{InteractionID: fresh.ID, Decision: "allowed-once", ExpectedRevision: snap.Revision, IdempotencyKey: "answer"})
	if err != nil {
		t.Fatal(err)
	}
	status, err := opened.GetOperation(t.Context(), receipt.OperationID)
	if err != nil || status.State != "completed" || runs.Load() != 0 || requests.Load() != 1 {
		t.Fatalf("answer ran work or lost its queryable status: %v", err)
	}
	snap, err = opened.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.Resume(t.Context(), sdk.ResumeCommand{TraceID: input.TraceID, ExpectedRevision: snap.Revision}); err != nil {
		t.Fatal(err)
	}
	after := waitConsumerApprovalState(t, opened, input.TraceID, "completed")
	if requests.Load() != 2 || runs.Load() != 1 || after.Traces[input.TraceID].Usage.ToolExecutions != 1 || after.Cursor <= before.Cursor {
		t.Fatalf("wrong execution totals: requests=%d tools=%d", requests.Load(), runs.Load())
	}
}
