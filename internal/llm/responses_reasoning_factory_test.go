package llm_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func reasoningFixtureItem(id, opaque string) string {
	encoded, _ := json.Marshal(map[string]any{"type": "reasoning", "id": id, "status": "completed", "summary": []any{}, "encrypted_content": opaque})
	return string(encoded)
}

func reasoningFixtureEvent(kind string, index int, item string) string {
	return fmt.Sprintf("data: {\"type\":\"response.output_item.%s\",\"output_index\":%d,\"item\":%s}\n\n", kind, index, item)
}

func reasoningFixtureTerminal(items ...string) string {
	return "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[" + strings.Join(items, ",") + "]}}\n\n"
}

// This characterization locks the bare v0.2.4 adapter's known limitation: it
// emits the real reasoning item but drops encrypted_content first seen at done.
// A changed upstream behavior must trigger review of the local supplement.
// Product correctness is asserted separately by the factory replay tests.
func TestP2FixedAdapterResponsesV024DoneOnlySignatureLimitation(t *testing.T) {
	item := reasoningFixtureItem("rs_bare", "synthetic-bare-private-data")
	body := reasoningFixtureEvent("added", 0, reasoningFixtureItem("rs_bare", "")) + reasoningFixtureEvent("done", 0, item) + reasoningFixtureTerminal(item)
	// p2ProbeServer also asserts exactly one physical HTTP call at cleanup.
	url, client, requests := p2ProbeServer(t, p2ProbeReply{true, body})
	zero, no := 0, false
	m, err := agenticopenai.NewResponsesModel(t.Context(), &agenticopenai.ResponsesConfig{APIKey: "synthetic-key", BaseURL: url, Model: "fixture", HTTPClient: client, MaxRetries: &zero, Store: &no})
	if err != nil {
		t.Fatal("bare adapter construction failed")
	}
	msg := p2ProbeInvoke(t, m, true, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
	if len(msg.ContentBlocks) != 1 || msg.ContentBlocks[0].Reasoning == nil {
		t.Fatal("pinned v0.2.4 must emit exactly one reasoning block")
	}
	block := msg.ContentBlocks[0]
	if fmt.Sprint(block.Extra["openai-item-id"]) != "rs_bare" {
		t.Error("pinned v0.2.4 reasoning item identity changed")
	}
	if block.Reasoning.Signature != "" {
		t.Error("pinned v0.2.4 done-only signature limitation changed; review product supplementation")
	}
	if msg.ResponseMeta == nil || msg.ResponseMeta.OpenAIExtension == nil || msg.ResponseMeta.OpenAIExtension.Status != "completed" {
		t.Error("pinned v0.2.4 terminal metadata changed")
	}
	request := p2ProbeRequest(t, requests)
	if request["stream"] != true || request["store"] != false || request["previous_response_id"] != nil || len(requests) != 0 {
		t.Error("bare adapter request contract or captured request count changed")
	}
}

func TestResponsesReasoningFactoryBoundaries(t *testing.T) {
	const opaque = "synthetic-boundary-private-data"
	empty := reasoningFixtureItem("rs_a", "")
	item := reasoningFixtureItem("rs_a", opaque)
	added := reasoningFixtureEvent("added", 0, empty)
	done := reasoningFixtureEvent("done", 0, item)
	terminal := reasoningFixtureTerminal(item)
	for _, tc := range []struct {
		name, body string
		limit      int
		invalid    bool
	}{
		{name: "done_only_signature", body: added + done + terminal},
		{name: "terminal_only_signature", body: added + reasoningFixtureEvent("done", 0, empty) + terminal},
		{name: "repeated_signature_once", body: reasoningFixtureEvent("added", 0, item) + done + terminal},
		{name: "no_added_item_identity_in_done", body: done + terminal},
		{name: "missing_item_id", body: reasoningFixtureEvent("added", 0, reasoningFixtureItem("", opaque)) + terminal, invalid: true},
		{name: "missing_output_index", body: "data: {\"type\":\"response.output_item.added\",\"item\":" + item + "}\n\n" + terminal, invalid: true},
		{name: "negative_output_index", body: reasoningFixtureEvent("added", -1, item) + terminal, invalid: true},
		{name: "changed_item_id", body: added + reasoningFixtureEvent("done", 0, reasoningFixtureItem("rs_b", opaque)) + terminal, invalid: true},
		{name: "changed_output_index", body: added + reasoningFixtureEvent("done", 1, item) + terminal, invalid: true},
		{name: "conflicting_signature", body: reasoningFixtureEvent("added", 0, reasoningFixtureItem("rs_a", "synthetic-other")) + done + terminal, invalid: true},
		{name: "missing_terminal", body: added + done, invalid: true},
		{name: "missing_final_item", body: added + done + reasoningFixtureTerminal(), invalid: true},
		{name: "terminal_without_emitted_item", body: terminal, invalid: true},
		{name: "duplicate_final_item", body: added + done + reasoningFixtureTerminal(item, item), invalid: true},
		{name: "event_after_terminal", body: added + done + terminal + done, invalid: true},
		{name: "incomplete_frame", body: added + done + strings.TrimSuffix(terminal, "\n\n"), invalid: true},
		{name: "capture_limit", body: added + done + terminal, limit: 64, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, client, _ := p2ProbeServer(t, p2ProbeReply{true, tc.body})
			catalog := llm.NewCatalog(nil)
			cfg := p2ResponsesConfig()
			cfg.Endpoint = url + "/v1"
			limit := tc.limit
			if limit == 0 {
				limit = 4096
			}
			p2ResponsesRegister(t, catalog, client, limit)
			p2OK(t, catalog.Register(cfg))
			m, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
			p2OK(t, err)
			var observed atomic.Int32
			usage := &usageObserver{}
			ctx := llm.WithUsageObservation(p2ChatContext(t, &observed), usage)
			msg, err := p2ChatInvoke(ctx, m, true, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
			if tc.invalid {
				if err == nil {
					t.Fatal("unproven reasoning replay was accepted")
				}
				if strings.Contains(err.Error(), opaque) {
					t.Fatal("private replay value escaped in error")
				}
				p2Code(t, err, product.CodeInvalidArgument)
			} else {
				if err != nil {
					t.Fatal("valid reasoning replay failed")
				}
				if len(msg.ContentBlocks) != 1 || msg.ContentBlocks[0].Reasoning == nil || msg.ContentBlocks[0].Reasoning.Signature != opaque {
					t.Fatal("opaque reasoning was not supplemented exactly once")
				}
				public, _ := json.Marshal(msg.Extra)
				if strings.Contains(string(public), opaque) {
					t.Fatal("private replay value escaped in message diagnostics")
				}
			}
			if observed.Load() != 1 {
				t.Fatal("physical observation count changed")
			}
			usage.mu.Lock()
			defer usage.mu.Unlock()
			public, _ := json.Marshal(usage.snapshots)
			if strings.Contains(string(public), opaque) || strings.Contains(string(public), "rs_a") {
				t.Fatal("private replay metadata escaped in usage diagnostics")
			}
		})
	}
}

// Stream order differs from output-array order, so output_index cannot be used
// as Eino's block index. Only real provider identity can join these records.
func TestResponsesReasoningFactoryMultipleItems(t *testing.T) {
	a := reasoningFixtureItem("rs_a", "synthetic-a")
	b := reasoningFixtureItem("rs_b", "synthetic-b")
	body := reasoningFixtureEvent("added", 1, reasoningFixtureItem("rs_b", "")) +
		reasoningFixtureEvent("added", 0, reasoningFixtureItem("rs_a", "")) +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_a\",\"output_index\":0,\"summary_index\":0,\"delta\":\"summary a\"}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_b\",\"output_index\":1,\"summary_index\":0,\"delta\":\"summary b\"}\n\n" +
		reasoningFixtureEvent("done", 0, a) + reasoningFixtureEvent("done", 1, b) + reasoningFixtureTerminal(a, b)
	url, client, requests := p2ProbeServer(t, p2ProbeReply{true, body}, p2ProbeReply{false, p2ResponsesResponse("completed", "", p2ResponsesTextOutput("done"), true)})
	catalog := llm.NewCatalog(nil)
	cfg := p2ResponsesConfig()
	cfg.Endpoint = url + "/v1"
	p2ResponsesRegister(t, catalog, client, 4096)
	p2OK(t, catalog.Register(cfg))
	m, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	var observed atomic.Int32
	ctx := p2ChatContext(t, &observed)
	input := []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}
	msg, err := p2ChatInvoke(ctx, m, true, input)
	if err != nil {
		t.Fatal("multiple-item stream failed")
	}
	if len(msg.ContentBlocks) != 2 {
		t.Fatal("reasoning item count changed")
	}
	for i, want := range []string{"b", "a"} {
		r := msg.ContentBlocks[i].Reasoning
		if r == nil || r.Text != "summary "+want || r.Signature != "synthetic-"+want {
			t.Fatal("reasoning summary/signature association changed")
		}
	}
	_, err = m.Generate(ctx, append(input, msg, schema.UserAgenticMessage("continue")))
	if err != nil {
		t.Fatal("multiple-item replay failed")
	}
	p2ProbeRequest(t, requests)
	replay := p2ProbeRequest(t, requests)
	count := 0
	for _, raw := range replay["input"].([]any) {
		item := raw.(map[string]any)
		if item["type"] == "reasoning" {
			count++
			id, _ := item["id"].(string)
			if (id != "rs_a" && id != "rs_b") || item["encrypted_content"] != "synthetic-"+strings.TrimPrefix(id, "rs_") {
				t.Fatal("real item identity was not preserved on replay")
			}
		}
	}
	if count != 2 || observed.Load() != 2 {
		t.Fatal("replay or HTTP count changed")
	}
}
