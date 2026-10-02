package workflowagent

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestWorkflowSubflowConcurrencyUsesExistingInvocationLimit(t *testing.T) {
	var tools atomic.Int32
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	m := testkit.NewFake(testkit.Step{Gate: gate, Text: "complete", Repeat: true})
	child := modelThenTool()
	d := child
	d.Name = "many-subflows"
	d.Nodes = []WorkflowNode{{ID: "s", Type: "start"}, {ID: "e", Type: "end", Inputs: map[string]WorkflowValue{"result": {Literal: json.RawMessage(`"done"`)}}}}
	d.Edges = nil
	for i := 0; i < config.SubagentConcurrency*2; i++ {
		id := fmt.Sprintf("child-%d", i)
		d.Nodes = append(d.Nodes, WorkflowNode{ID: id, Type: "subflow", Subflow: child.Name + "@" + child.Version})
		d.Edges = append(d.Edges, WorkflowEdge{From: "s", To: id}, WorkflowEdge{From: id, To: "e"})
	}
	opts := testOptions(t, d, m, &tools)
	opts.Subflows = map[string]WorkflowDefinition{child.Name + "@" + child.Version: child}
	w := newWorkflow(t, opts)
	submit(t, w)
	end := time.Now().Add(5 * time.Second)
	for m.Calls() < config.SubagentConcurrency && time.Now().Before(end) {
		time.Sleep(time.Millisecond)
	}
	w.mu.Lock()
	live := 0
	rejected := false
	for _, n := range w.state.Nodes {
		if n.Kind == "subflow" && n.State == "accepted" {
			live++
		}
		if n.Kind == "subflow" && n.ErrorCode == product.CodeBudgetExhausted {
			rejected = true
		}
	}
	w.mu.Unlock()
	if live > config.SubagentConcurrency {
		t.Errorf("admitted %d live subflows, limit %d", live, config.SubagentConcurrency)
	}
	if !rejected {
		t.Error("fifth invocation was not rejected before it ran")
	}
	release()
	s := waitStopped(t, w)
	if s.State != "failed" || s.ErrorCode != product.CodeBudgetExhausted || m.Calls() > config.SubagentConcurrency {
		t.Errorf("subflow limit state=%s code=%s requests=%d", s.State, s.ErrorCode, m.Calls())
	}
}
