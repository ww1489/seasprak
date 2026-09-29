package eino

import (
	"context"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/dynamictool/toolsearch"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
)

// NewToolSearchMatcher reuses Eino's ordinary matching tool without installing
// its history-driven visibility middleware. Product selection remains durable.
func NewToolSearchMatcher(ctx context.Context, infos []*schema.ToolInfo) (tool.InvokableTool, error) {
	inventory := make([]tool.BaseTool, 0, len(infos))
	for _, info := range infos {
		inventory = append(inventory, searchInfo{info})
	}
	middleware, err := toolsearch.NewTyped[*schema.AgenticMessage](ctx, &toolsearch.Config{DynamicTools: inventory})
	if err != nil {
		return nil, err
	}
	_, assembled, err := middleware.BeforeAgent(ctx, &adk.ChatModelAgentContext{})
	if err != nil {
		return nil, err
	}
	for _, candidate := range assembled.Tools {
		info, err := candidate.Info(ctx)
		if err != nil {
			return nil, err
		}
		if info.Name == "tool_search" {
			if matcher, ok := candidate.(tool.InvokableTool); ok {
				return matcher, nil
			}
		}
	}
	return nil, product.NewError(product.CodeResourceUnavailable, "framework tool search is unavailable")
}

type searchInfo struct{ info *schema.ToolInfo }

func (s searchInfo) Info(context.Context) (*schema.ToolInfo, error) { return s.info, nil }
