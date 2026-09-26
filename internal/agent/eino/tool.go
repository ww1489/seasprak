package eino

import (
	"context"
	"encoding/json"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
)

type pipelineTool struct {
	exec  *tools.Executor
	info  *schema.ToolInfo
	scope agent.ExecutionScope
}

func NewPipelineTool(info *schema.ToolInfo, exec *tools.Executor, scope agent.ExecutionScope) tool.InvokableTool {
	return &pipelineTool{info: info, exec: exec, scope: scope}
}

// NewPipelineToolForInterface selects the static, generation-bound Eino interface.
func NewPipelineToolForInterface(info *schema.ToolInfo, exec *tools.Executor, scope agent.ExecutionScope, kind string) (tool.BaseTool, error) {
	base := &pipelineTool{info: info, exec: exec, scope: scope}
	switch kind {
	case "", "invokable":
		return base, nil
	case "enhanced-invokable":
		return &enhancedPipelineTool{base}, nil
	case "streamable", "enhanced-streamable":
		return nil, product.NewError(product.CodeResourceUnavailable, "streaming tool lifecycle is unavailable")
	default:
		return nil, product.NewError(product.CodeInvalidArgument, "tool interface is unsupported")
	}
}

type enhancedPipelineTool struct{ *pipelineTool }

func (t *enhancedPipelineTool) InvokableRun(ctx context.Context, args *schema.ToolArgument, _ ...tool.Option) (*schema.ToolResult, error) {
	if args == nil {
		return nil, product.NewError(product.CodeInvalidArgument, "tool arguments are missing")
	}
	content, err := t.run(ctx, args.Text)
	if err != nil {
		return nil, err
	}
	return &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: content}}}, nil
}

func (t *pipelineTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

func (t *pipelineTool) InvokableRun(ctx context.Context, arguments string, _ ...tool.Option) (string, error) {
	return t.run(ctx, arguments)
}

func (t *pipelineTool) run(ctx context.Context, arguments string) (string, error) {
	out, err := t.exec.Run(ctx, ScopeFromContext(ctx, t.scope), compose.GetToolCallID(ctx), t.info.Name, arguments)
	if err != nil {
		return "", err
	}
	if out.Status != "succeeded" {
		raw, _ := json.Marshal(out)
		return string(raw), nil
	}
	return out.Content, nil
}
