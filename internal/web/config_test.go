package web

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions"
)

// rewriteStartup updates the trusted startup fixture without changing its model.
func rewriteStartup(t *testing.T, c Config, change func(*startupConfig)) {
	t.Helper()
	b, err := os.ReadFile(c.ConfigPath)
	if err != nil {
		t.Fatal("config read failed")
	}
	var conf startupConfig
	if err = json.Unmarshal(b, &conf); err != nil {
		t.Fatal("config parse failed")
	}
	change(&conf)
	if b, err = json.Marshal(conf); err != nil || os.WriteFile(c.ConfigPath, b, 0600) != nil {
		t.Fatal("config write failed")
	}
}

// withTools rewrites the test startup file with the given tools allowlist.
func withTools(t *testing.T, c Config, names []string) {
	t.Helper()
	rewriteStartup(t, c, func(conf *startupConfig) { conf.Tools = names })
}

func startupEchoWorkflow(name string) agent.WorkflowDefinition {
	return agent.WorkflowDefinition{
		Name: name, Version: "v1", Description: "echo input", Source: "trusted-startup", FormatVersion: agent.WorkflowFormatV1,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}`),
		Nodes: []agent.WorkflowNode{
			{ID: "start", Type: agent.WorkflowNodeStart},
			{ID: "end", Type: agent.WorkflowNodeEnd, Inputs: map[string]agent.WorkflowValue{"answer": {Ref: &agent.WorkflowRef{Node: "start", Field: "topic"}}}},
		},
		Edges: []agent.WorkflowEdge{{From: "start", To: "end"}},
	}
}

func TestStartupRegistersAgentsAndExecutableWorkflows(t *testing.T) {
	c := testConfig(t)
	rewriteStartup(t, c, func(conf *startupConfig) {
		conf.Profile = sessions.ProfileMemory
		conf.Agents = []startupAgent{{Name: "reviewer", Version: "r1", Description: "reviews", Instruction: "Review carefully.", Delegable: true}}
		conf.Workflows = []startupWorkflow{{WorkflowDefinition: startupEchoWorkflow("echo-flow"), Delegable: true}}
	})
	opts, err := loadOptions(t.Context(), c)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(opts.Agents) != 2 || opts.Agents[0].Name != "reviewer" || opts.Agents[0].Model != nil || !opts.Agents[0].Delegable || opts.Agents[1].Name != "echo-flow" || opts.Agents[1].Workflow == nil {
		t.Fatalf("registered targets=%+v", opts.Agents)
	}
	catalog, err := sessions.NewCatalog(opts)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	defer catalog.Close(context.Background())
	created, err := catalog.Create(t.Context(), sessions.CatalogCreateRequest{IdempotencyKey: "create", Workspace: c.Workspace, ModelRef: sessions.DefaultModelRef})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s, err := catalog.Writer(t.Context(), created.Snapshot.SessionID)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	caps, err := s.Capabilities(t.Context())
	if err != nil || len(caps.Agents) != 3 || caps.Agents[0].Name != "echo-flow" || caps.Agents[1].Name != "main" || caps.Agents[2].Name != "reviewer" {
		t.Fatalf("capabilities=%+v err=%v", caps, err)
	}
	receipt, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", TargetAgent: "echo-flow", Content: json.RawMessage(`{"input":{"topic":"你好"}}`)})
	if err != nil {
		t.Fatalf("submit workflow: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, snapErr := s.Snapshot(t.Context())
		if snapErr != nil {
			t.Fatal(snapErr)
		}
		if tr := snap.Traces[receipt.TraceID]; tr != nil && tr.Settled {
			result := ""
			for _, message := range snap.Messages {
				if message.Scope.TraceID == receipt.TraceID && message.Custom != nil && message.Custom.CustomType == "workflow_result" {
					result = string(message.Custom.Details)
				}
			}
			if tr.State != "completed" || tr.Usage.LogicalModelCalls != 0 || len(snap.WorkflowNodes) != 0 || !strings.Contains(result, `"answer":"你好"`) {
				t.Fatalf("workflow trace=%+v nodes=%+v result=%s", tr, snap.WorkflowNodes, result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup workflow did not settle")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartupRejectsInvalidTargetInventoryBeforeCreatingStateRoot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*startupConfig)
	}{
		{"reserved-main", func(c *startupConfig) { c.Agents = []startupAgent{{Name: "main", Version: "v2", Instruction: "x"}} }},
		{"unknown-agent-tool", func(c *startupConfig) {
			c.Agents = []startupAgent{{Name: "worker", Version: "v1", Instruction: "x", Tools: []string{"missing"}}}
		}},
		{"invalid-workflow", func(c *startupConfig) {
			wf := startupEchoWorkflow("broken")
			wf.Nodes = append(wf.Nodes, agent.WorkflowNode{ID: "bad", Type: "arbitrary-http"})
			c.Workflows = []startupWorkflow{{WorkflowDefinition: wf}}
		}},
		{"missing-subflow", func(c *startupConfig) {
			wf := startupEchoWorkflow("parent")
			wf.Nodes = []agent.WorkflowNode{{ID: "start", Type: agent.WorkflowNodeStart}, {ID: "sub", Type: agent.WorkflowNodeSubflow, Subflow: "missing@v1"}, {ID: "end", Type: agent.WorkflowNodeEnd}}
			wf.Edges = []agent.WorkflowEdge{{From: "start", To: "sub"}, {From: "sub", To: "end"}}
			c.Workflows = []startupWorkflow{{WorkflowDefinition: wf}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			rewriteStartup(t, c, tc.change)
			_, err := loadOptions(t.Context(), c)
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
				t.Fatalf("err=%v", err)
			}
			if _, statErr := os.Stat(c.StateRoot); !os.IsNotExist(statErr) {
				t.Fatal("rejected target inventory created the state root")
			}
		})
	}
}

func TestStartupApprovalToolsMustBeEnabledSessionOwnedTools(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tools     []string
		approvals []string
		ok        bool
	}{
		{"enabled", []string{"write_todos"}, []string{"write_todos"}, true},
		{"not-enabled", nil, []string{"write_todos"}, false},
		{"unknown", []string{"write_todos"}, []string{"other"}, false},
		{"duplicate", []string{"write_todos"}, []string{"write_todos", "write_todos"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			rewriteStartup(t, c, func(conf *startupConfig) {
				conf.Tools = tc.tools
				conf.ApprovalTools = tc.approvals
			})
			opts, err := loadOptions(t.Context(), c)
			if tc.ok {
				if err != nil || len(opts.Tools) != 1 || opts.Tools[0].Execution.RequestedGrantRef != "one-operation" {
					t.Fatalf("opts=%+v err=%v", opts.Tools, err)
				}
				return
			}
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
				t.Fatalf("err=%v", err)
			}
			if _, statErr := os.Stat(c.StateRoot); !os.IsNotExist(statErr) {
				t.Fatal("rejected approval inventory created the state root")
			}
		})
	}
}

func TestStartupToolsAllowOnlySessionOwnedBuiltins(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools []string
	}{
		{"unknown", []string{"no_such_tool"}},
		{"host-process", []string{"execute"}},
		{"host-file", []string{"read_file"}},
		{"duplicate", []string{"write_todos", "write_todos"}},
		{"empty-name", []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			withTools(t, c, tc.tools)
			_, err := loadOptions(context.Background(), c)
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
				t.Fatalf("err=%v", err)
			}
			if _, statErr := os.Stat(c.StateRoot); !os.IsNotExist(statErr) {
				t.Fatal("rejected configuration created the state root")
			}
		})
	}
}

func TestStartupToolsInstallWriteTodos(t *testing.T) {
	c := testConfig(t)
	withTools(t, c, []string{"write_todos"})
	opts, err := loadOptions(context.Background(), c)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(opts.Tools) != 1 || opts.Tools[0].Name != "write_todos" || opts.Tools[0].Execution.BackendID != "todo-operations" {
		t.Fatalf("tools=%+v", opts.Tools)
	}
	// Without the field no tool is installed.
	c = testConfig(t)
	if opts, err = loadOptions(context.Background(), c); err != nil || len(opts.Tools) != 0 {
		t.Fatalf("default tools=%d err=%v", len(opts.Tools), err)
	}
}
