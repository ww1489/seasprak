package workflowagent

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

type configuredFake struct {
	model.AgenticModel
	version string
}

func (m configuredFake) Configuration() llm.ModelConfig { return llm.ModelConfig{Version: m.version} }
func TestWorkflowOpenRejectsPrincipalAndBindingChangesBeforeExecution(t *testing.T) {
	var tool atomic.Int32
	fake := testkit.NewFake()
	opts := testOptions(t, modelThenTool(), configuredFake{AgenticModel: fake, version: "model-v1"}, &tool)
	w := newWorkflow(t, opts)
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, code string
		change     func(*WorkflowOptions)
	}{{"principal", product.CodePermissionDenied, func(o *WorkflowOptions) { o.Principal = "other" }}, {"readonly_principal", product.CodePermissionDenied, func(o *WorkflowOptions) { o.Principal = "other"; o.ReadOnly = true }}, {"model_version", product.CodeIncompatibleVersion, func(o *WorkflowOptions) {
		o.Models = map[string]model.AgenticModel{"chosen": configuredFake{AgenticModel: fake, version: "model-v2"}}
	}}, {"missing_model_binding", product.CodeIncompatibleVersion, func(o *WorkflowOptions) { o.Models = nil }}, {"tool_version", product.CodeIncompatibleVersion, func(o *WorkflowOptions) { o.Tools = append(o.Tools[:0:0], o.Tools...); o.Tools[0].Version = "v2" }}} {
		t.Run(tc.name, func(t *testing.T) {
			changed := opts
			tc.change(&changed)
			opened, err := OpenWorkflowAgent(t.Context(), changed)
			if opened != nil {
				opened.Close(context.Background())
			}
			requireCode(t, err, tc.code)
			if fake.Calls() != 0 || tool.Load() != 0 {
				t.Fatal("rejected open executed")
			}
		})
	}
	ro := opts
	ro.ReadOnly = true
	ro.Models = nil
	ro.Tools = nil
	ro.Definition.Name = "unavailable"
	opened, err := OpenWorkflowAgent(t.Context(), ro)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close(context.Background())
	s, err := opened.Snapshot(t.Context())
	if err != nil || s.State != "created" || fake.Calls() != 0 {
		t.Fatalf("readonly browsing %+v %v", s, err)
	}
}
