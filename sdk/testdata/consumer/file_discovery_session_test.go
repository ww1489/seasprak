package consumer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/sdk"
)

// Public-only host backend: no SDK internals or host filesystem access.
type consumerDiscoveryFiles struct {
	publicFiles
	mu       sync.Mutex
	lists    int
	searches []sdk.SearchRequest
}

func (*consumerDiscoveryFiles) ExecutionCapabilities(context.Context) (sdk.BackendCapabilities, error) {
	return sdk.BackendCapabilities{BackendID: "consumer-discovery", Version: "v1", EnvironmentID: "consumer-memory", SupportedModes: []string{"workspace-write"}, Enforcement: "full", RuntimeDataWriteProtected: true}, nil
}
func (b *consumerDiscoveryFiles) List(ctx context.Context, r sdk.ListRequest) (sdk.ListResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lists++
	if err := ctx.Err(); err != nil {
		return sdk.ListResult{}, err
	}
	if r.Root != "root" {
		return sdk.ListResult{}, sdk.NewError(sdk.CodeInvalidArgument, "unexpected root")
	}
	return sdk.ListResult{Entries: []sdk.FileEntry{{Identity: "root/z", Name: "z", Kind: "file"}, {Identity: "root/a", Name: "a", Kind: "file"}}}, nil
}
func (b *consumerDiscoveryFiles) Search(ctx context.Context, r sdk.SearchRequest) (sdk.SearchResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.searches = append(b.searches, r)
	if err := ctx.Err(); err != nil {
		return sdk.SearchResult{}, err
	}
	if r.Kind != "grep" || r.Root != "root/a" || r.OutputMode != "count" || r.Query != "needle" {
		return sdk.SearchResult{}, sdk.NewError(sdk.CodeInvalidArgument, "unexpected search binding")
	}
	return sdk.SearchResult{Matches: []sdk.SearchMatch{{Identity: r.Root, Line: 2, Preview: "needle"}, {Identity: r.Root, Line: 1, Preview: "needle"}}}, nil
}

type consumerDiscoveryModel struct {
	mu              sync.Mutex
	calls, observed int
}

func consumerDiscoveryResult(in []*schema.AgenticMessage, id string) (string, error) {
	found := 0
	text := ""
	for _, msg := range in {
		for _, block := range msg.ContentBlocks {
			result := block.FunctionToolResult
			if result == nil || result.CallID != id {
				continue
			}
			found++
			for _, part := range result.Content {
				if part.Text != nil {
					text += part.Text.Text
				}
			}
		}
	}
	if found != 1 || !json.Valid([]byte(text)) {
		return "", fmt.Errorf("missing structured paired result %s", id)
	}
	return text, nil
}
func (m *consumerDiscoveryModel) Generate(_ context.Context, in []*schema.AgenticMessage, _ ...model.Option) (*schema.AgenticMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	name, id, args := "ls", "consumer-list", `{"root":"root"}`
	switch m.calls {
	case 1:
	case 2:
		raw, err := consumerDiscoveryResult(in, "consumer-list")
		if err != nil {
			return nil, err
		}
		var page struct {
			Entries   []sdk.FileEntry
			Returned  int
			Truncated bool
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			return nil, err
		}
		if page.Returned != 2 || page.Truncated || len(page.Entries) != 2 || page.Entries[0].Identity != "root/a" || page.Entries[1].Identity != "root/z" {
			return nil, fmt.Errorf("unsorted public directory result")
		}
		m.observed++
		body, _ := json.Marshal(map[string]any{"root": page.Entries[0].Identity, "query": "needle", "output_mode": "count"})
		name, id, args = "grep", "consumer-search", string(body)
	case 3:
		raw, err := consumerDiscoveryResult(in, "consumer-search")
		if err != nil {
			return nil, err
		}
		var page struct {
			Counts []struct {
				Identity string
				Count    int
			}
			Returned  int
			Truncated bool
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			return nil, err
		}
		if page.Returned != 1 || page.Truncated || len(page.Counts) != 1 || page.Counts[0].Identity != "root/a" || page.Counts[0].Count != 2 {
			return nil, fmt.Errorf("invalid public grep count result")
		}
		m.observed++
		return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "done"})}, Extra: map[string]any{"seasprak.finish": "stop"}}, nil
	default:
		return nil, fmt.Errorf("unexpected model retry")
	}
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: id, Name: name, Arguments: args})}, Extra: map[string]any{"seasprak.finish": "tool_calls"}}, nil
}
func (m *consumerDiscoveryModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	out, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{out}), nil
}
func TestSDKConsumerFileDiscoveryThroughSession(t *testing.T) {
	files := &consumerDiscoveryFiles{}
	m := &consumerDiscoveryModel{}
	s, err := sdk.CreateAgentSession(t.Context(), sdk.SessionOptions{SessionID: "consumer-discovery", Workspace: t.TempDir(), StateRoot: "memory", Profile: sdk.ProfileMemory, Model: m, Tools: sdk.NewBuiltinDefinitions(sdk.BuiltinOptions{}), Operations: sdk.Operations{Files: files}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	input, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"find a file and search it"}`)})
	if err != nil {
		t.Fatal(err)
	}
	view := waitConsumerApprovalState(t, s, input.TraceID, "completed")
	m.mu.Lock()
	defer m.mu.Unlock()
	files.mu.Lock()
	defer files.mu.Unlock()
	if m.calls != 3 || m.observed != 2 || files.lists != 1 || len(files.searches) != 1 || view.Traces[input.TraceID].Usage.ToolExecutions != 2 || len(view.Calls) != 2 {
		t.Fatalf("model=%d observed=%d list=%d search=%d", m.calls, m.observed, files.lists, len(files.searches))
	}
	for _, call := range view.Calls {
		if call.Observation == nil || call.Observation.Status != "succeeded" || !call.Observation.Executed || call.Observation.SideEffect != "none" {
			t.Fatalf("call=%+v", call)
		}
	}
}
