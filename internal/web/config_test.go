package web

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
	"github.com/ww1489/seasprak/internal/workflowagent"
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

func startupEchoWorkflow(name string) workflowagent.WorkflowDefinition {
	return workflowagent.WorkflowDefinition{
		Name: name, Version: "v1", Description: "echo input", Source: "trusted-startup", FormatVersion: workflowagent.WorkflowFormatV1,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}`),
		Nodes: []workflowagent.WorkflowNode{
			{ID: "start", Type: workflowagent.WorkflowNodeStart},
			{ID: "end", Type: workflowagent.WorkflowNodeEnd, Inputs: map[string]workflowagent.WorkflowValue{"answer": {Ref: &workflowagent.WorkflowRef{Node: "start", Field: "topic"}}}},
		},
		Edges: []workflowagent.WorkflowEdge{{From: "start", To: "end"}},
	}
}

func TestStartupRegistersAgentsAndExecutableWorkflows(t *testing.T) {
	c := testConfig(t)
	rewriteStartup(t, c, func(conf *startupConfig) {
		conf.Profile = codeagent.ProfileMemory
		conf.Agents = []startupAgent{{Name: "reviewer", Version: "r1", Description: "reviews", Instruction: "Review carefully.", Delegable: true}}
		conf.Workflows = []startupWorkflow{{WorkflowDefinition: startupEchoWorkflow("echo-flow")}}
	})
	opts, err := loadOptions(t.Context(), c)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(opts.Code.Agents) != 1 || opts.Code.Agents[0].Name != "reviewer" || opts.Code.Agents[0].Model != nil || !opts.Code.Agents[0].Delegable || len(opts.Workflows) != 1 {
		t.Fatal("ordinary and independent inventories were merged")
	}
	main := testkit.NewFake()
	opts.Code.Model = main
	s, err := codeagent.CreateAgentSession(t.Context(), opts.Code)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	caps, err := s.Capabilities(t.Context())
	if err != nil || len(caps.Agents) != 2 || caps.Agents[0].Name != "main" || caps.Agents[1].Name != "reviewer" {
		t.Fatalf("Code capabilities %+v %v", caps, err)
	}
	before, _ := s.Snapshot(t.Context())
	_, err = s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", TargetAgent: "echo-flow", Content: json.RawMessage(`{"input":{"topic":"你好"}}`)})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeUnsupportedCapability {
		t.Fatalf("legacy target accepted: %v", err)
	}
	after, _ := s.Snapshot(t.Context())
	if after.Revision != before.Revision || main.Calls() != 0 || len(after.Invocations) != 0 {
		t.Fatal("legacy target wrote state or ran")
	}
	wf := opts.Workflows["echo-flow@v1"]
	if wf.RunID != "" || wf.Definition.Name != "echo-flow" || wf.Definition.Version != "v1" || wf.Models[workflowagent.WorkflowModelBinding] == nil || len(wf.Models) != 1 || wf.Workspace != opts.Code.Workspace || wf.StateRoot != opts.Code.StateRoot || wf.Principal != localPrincipal || wf.GenerationFingerprint == opts.Code.GenerationFingerprint {
		t.Fatal("fixed startup workflow binding is incomplete")
	}
	w, err := workflowagent.CreateWorkflowAgent(t.Context(), wf)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	if _, err = w.SubmitInput(t.Context(), workflowagent.WorkflowInputCommand{Input: json.RawMessage(`{"topic":"你好"}`), Principal: localPrincipal}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, err := w.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if snap.ExecutionStopped {
			if snap.State != "completed" || snap.Usage.LogicalModelCalls != 0 || len(snap.WorkflowNodes) != 0 || !strings.Contains(string(snap.Result), `"answer":"你好"`) || main.Calls() != 0 {
				t.Fatalf("independent workflow state=%s result=%s", snap.State, snap.Result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("independent startup workflow did not exit")
		}
		time.Sleep(time.Millisecond)
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
			wf.Nodes = append(wf.Nodes, workflowagent.WorkflowNode{ID: "bad", Type: "arbitrary-http"})
			c.Workflows = []startupWorkflow{{WorkflowDefinition: wf}}
		}},
		{"missing-subflow", func(c *startupConfig) {
			wf := startupEchoWorkflow("parent")
			wf.Nodes = []workflowagent.WorkflowNode{{ID: "start", Type: "start"}, {ID: "sub", Type: "subflow", Subflow: "missing@v1"}, {ID: "end", Type: "end"}}
			wf.Edges = []workflowagent.WorkflowEdge{{From: "start", To: "sub"}, {From: "sub", To: "end"}}
			c.Workflows = []startupWorkflow{{WorkflowDefinition: wf}}
		}},
		{"duplicate-workflow", func(c *startupConfig) {
			wf := startupEchoWorkflow("dup")
			c.Workflows = []startupWorkflow{{WorkflowDefinition: wf}, {WorkflowDefinition: wf}}
		}},
		{"cycle", func(c *startupConfig) {
			wf := startupEchoWorkflow("cycle")
			wf.Nodes = []workflowagent.WorkflowNode{{ID: "start", Type: "start"}, {ID: "sub", Type: "subflow", Subflow: "cycle@v1"}, {ID: "end", Type: "end"}}
			wf.Edges = []workflowagent.WorkflowEdge{{From: "start", To: "sub"}, {From: "sub", To: "end"}}
			c.Workflows = []startupWorkflow{{WorkflowDefinition: wf}}
		}},
		{"unknown-model", func(c *startupConfig) {
			wf := startupEchoWorkflow("bad-model")
			wf.Nodes = []workflowagent.WorkflowNode{{ID: "start", Type: "start"}, {ID: "m", Type: "model", Model: "other", Prompt: "x"}, {ID: "end", Type: "end"}}
			wf.Edges = []workflowagent.WorkflowEdge{{From: "start", To: "m"}, {From: "m", To: "end"}}
			c.Workflows = []startupWorkflow{{WorkflowDefinition: wf}}
		}},
		{"unknown-tool", func(c *startupConfig) {
			wf := startupEchoWorkflow("bad-tool")
			wf.Nodes = []workflowagent.WorkflowNode{{ID: "start", Type: "start"}, {ID: "t", Type: "tool", Tool: "missing"}, {ID: "end", Type: "end"}}
			wf.Edges = []workflowagent.WorkflowEdge{{From: "start", To: "t"}, {From: "t", To: "end"}}
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
			if _, err := os.Stat(c.StateRoot); !os.IsNotExist(err) {
				t.Fatal("invalid inventory created state root")
			}
		})
	}
}

func TestStartupWorkflowDelegableFieldIsRejectedBeforeStateRoot(t *testing.T) {
	c := testConfig(t)
	raw, err := os.ReadFile(c.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var conf map[string]any
	if err = json.Unmarshal(raw, &conf); err != nil {
		t.Fatal(err)
	}
	wf, _ := json.Marshal(startupEchoWorkflow("old"))
	var old map[string]any
	json.Unmarshal(wf, &old)
	old["delegable"] = true
	conf["workflows"] = []any{old}
	raw, _ = json.Marshal(conf)
	if err = os.WriteFile(c.ConfigPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = loadOptions(t.Context(), c)
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
		t.Fatalf("legacy delegable accepted: %v", err)
	}
	if _, err = os.Stat(c.StateRoot); !os.IsNotExist(err) {
		t.Fatal("legacy declaration created state root")
	}
}

func TestStartupCodeAndWorkflowFingerprintsAreIndependent(t *testing.T) {
	c := testConfig(t)
	rewriteStartup(t, c, func(conf *startupConfig) {
		conf.Agents = []startupAgent{{Name: "reviewer", Version: "r1", Instruction: "original"}}
		conf.Workflows = []startupWorkflow{{WorkflowDefinition: startupEchoWorkflow("wf")}}
	})
	first, err := loadOptions(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	rewriteStartup(t, c, func(conf *startupConfig) { conf.Workflows[0].Description = "changed workflow" })
	second, err := loadOptions(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if first.Code.GenerationFingerprint != second.Code.GenerationFingerprint || first.Workflows["wf@v1"].GenerationFingerprint == second.Workflows["wf@v1"].GenerationFingerprint {
		t.Fatal("workflow declaration changed Code generation or failed to freeze itself")
	}
	rewriteStartup(t, c, func(conf *startupConfig) { conf.Agents[0].Instruction = "changed agent" })
	third, err := loadOptions(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if second.Code.GenerationFingerprint == third.Code.GenerationFingerprint || second.Workflows["wf@v1"].GenerationFingerprint != third.Workflows["wf@v1"].GenerationFingerprint {
		t.Fatal("ordinary declaration changed workflow fingerprint")
	}
	if reflect.DeepEqual(first.Workflows["wf@v1"].Definition, second.Workflows["wf@v1"].Definition) {
		t.Fatal("changed definition was not frozen separately")
	}
}

func TestStartupWorkflowOptionsOwnTheirMutableDeclarations(t *testing.T) {
	c := testConfig(t)
	rewriteStartup(t, c, func(conf *startupConfig) {
		conf.Tools = []string{"write_todos"}
		conf.Workflows = []startupWorkflow{{WorkflowDefinition: startupEchoWorkflow("first")}, {WorkflowDefinition: startupEchoWorkflow("second")}}
	})
	opts, err := loadOptions(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	first, second := opts.Workflows["first@v1"], opts.Workflows["second@v1"]
	codeSchema := append(json.RawMessage(nil), opts.Code.Tools[0].Schema...)
	secondSchema := append(json.RawMessage(nil), second.Tools[0].Schema...)
	first.Tools[0].Name = "mutated"
	first.Tools[0].Schema[0] = '?'
	if opts.Code.Tools[0].Name != "write_todos" || second.Tools[0].Name != "write_todos" || !reflect.DeepEqual(opts.Code.Tools[0].Schema, codeSchema) || !reflect.DeepEqual(second.Tools[0].Schema, secondSchema) {
		t.Fatal("workflow startup options share mutable tool declarations with another owner")
	}
	first.Definition.InputSchema[0] = '?'
	first.Definition.Nodes[0].ID = "changed"
	first.Subflows["second@v1"].InputSchema[0] = '?'
	delete(first.Models, workflowagent.WorkflowModelBinding)
	if second.Definition.InputSchema[0] != '{' || second.Definition.Nodes[0].ID != "start" || second.Subflows["first@v1"].Nodes[0].ID != "start" || second.Subflows["second@v1"].InputSchema[0] != '{' || second.Models[workflowagent.WorkflowModelBinding] == nil {
		t.Fatal("workflow startup options share mutable definition or model maps")
	}
}

func TestStartupApprovalToolsMustBeEnabledSessionOwnedTools(t *testing.T) {
	for _, tc := range []struct {
		name             string
		tools, approvals []string
		ok               bool
	}{
		{"enabled", []string{"write_todos"}, []string{"write_todos"}, true},
		{"not-enabled", nil, []string{"write_todos"}, false},
		{"unknown", []string{"write_todos"}, []string{"other"}, false},
		{"duplicate", []string{"write_todos"}, []string{"write_todos", "write_todos"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			rewriteStartup(t, c, func(conf *startupConfig) { conf.Tools = tc.tools; conf.ApprovalTools = tc.approvals })
			opts, err := loadOptions(t.Context(), c)
			if tc.ok {
				if err != nil || len(opts.Code.Tools) != 1 || opts.Code.Tools[0].Execution.RequestedGrantRef != "one-operation" {
					t.Fatalf("opts=%+v err=%v", opts.Code.Tools, err)
				}
				return
			}
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
				t.Fatalf("err=%v", err)
			}
			if _, err := os.Stat(c.StateRoot); !os.IsNotExist(err) {
				t.Fatal("rejected approval inventory created root")
			}
		})
	}
}

func TestStartupToolsAllowOnlySessionOwnedBuiltins(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools []string
	}{
		{"unknown", []string{"no_such_tool"}}, {"host-process", []string{"execute"}}, {"host-file", []string{"read_file"}}, {"duplicate", []string{"write_todos", "write_todos"}}, {"empty-name", []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			withTools(t, c, tc.tools)
			_, err := loadOptions(t.Context(), c)
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
				t.Fatalf("err=%v", err)
			}
			if _, err := os.Stat(c.StateRoot); !os.IsNotExist(err) {
				t.Fatal("rejected tools created root")
			}
		})
	}
}
func TestStartupToolsInstallWriteTodos(t *testing.T) {
	c := testConfig(t)
	withTools(t, c, []string{"write_todos"})
	opts, err := loadOptions(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.Code.Tools) != 1 || opts.Code.Tools[0].Name != "write_todos" || opts.Code.Tools[0].Execution.BackendID != "todo-operations" {
		t.Fatalf("tools=%+v", opts.Code.Tools)
	}
	c = testConfig(t)
	if opts, err = loadOptions(context.Background(), c); err != nil || len(opts.Code.Tools) != 0 {
		t.Fatalf("default tools=%d err=%v", len(opts.Code.Tools), err)
	}
}
