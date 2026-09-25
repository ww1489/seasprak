package consumer_test

import (
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/sdk"
)

func TestPublicMessagePayloadsAndEnums(t *testing.T) {
	messages := []sdk.AgentMessage{
		{Kind: sdk.KindUser, Standard: schema.UserAgenticMessage("user")},
		{Kind: sdk.KindAssistant, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant}},
		{Kind: sdk.KindToolResult, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: "call", Name: "tool"})}}},
		{Kind: sdk.KindCustom, Custom: &sdk.CustomMessage{CustomType: "notice", Display: true}},
		{Kind: sdk.KindCommand, Command: &sdk.CommandMessage{Name: "status"}},
		{Kind: sdk.KindCompactionSummary, Summary: &sdk.SummaryMessage{Text: "compacted"}},
		{Kind: sdk.KindBranchSummary, Summary: &sdk.SummaryMessage{Text: "branch"}},
		{Kind: sdk.KindOpaque, Opaque: &sdk.OpaqueMessage{Raw: json.RawMessage(`{"future":true}`), RequiredForModel: true}},
	}
	wantKinds := []string{"user", "assistant", "tool_result", "custom", "command", "compaction_summary", "branch_summary", "opaque"}
	for i, msg := range messages {
		msg.ID = "message"
		msg.Status = sdk.StatusComplete
		msg.Source = sdk.SourceRef{Kind: sdk.SourceHuman}
		if string(msg.Kind) != wantKinds[i] {
			t.Fatalf("kind = %q, want %q", msg.Kind, wantKinds[i])
		}
		if err := msg.Validate(); err != nil {
			t.Fatalf("validate %s: %v", msg.Kind, err)
		}
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		var restored sdk.AgentMessage
		if err := json.Unmarshal(raw, &restored); err != nil {
			t.Fatal(err)
		}
		if err := restored.Validate(); err != nil || restored.Kind != msg.Kind {
			t.Fatalf("restored %s: %v", msg.Kind, err)
		}
	}
	if sdk.StatusComplete != "complete" || sdk.StatusIncomplete != "incomplete" {
		t.Fatal("message status values changed")
	}
	sources := []sdk.SourceKind{sdk.SourceHuman, sdk.SourceDirectParent, sdk.SourceExtension, sdk.SourceTool, sdk.SourceModel, sdk.SourceResource, sdk.SourceImported}
	wantSources := []string{"human", "direct_parent", "extension", "tool", "model", "resource", "imported"}
	for i, source := range sources {
		if string(source) != wantSources[i] {
			t.Fatalf("source = %q, want %q", source, wantSources[i])
		}
	}
}
