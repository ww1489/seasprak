package codeagent

import (
	"context"

	"github.com/cloudwego/eino/components/model"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
)

// Capabilities lists what this session can actually execute. Targets come from
// the immutable startup registry; nothing here reflects a runtime reload.
type Capabilities struct {
	Agents []agent.AgentInfo
	Tools  []string
}

// Capabilities returns the registered targets and generation tool names.
func (s *AgentSession) Capabilities(ctx context.Context) (Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return Capabilities{}, err
	}
	out := Capabilities{Agents: s.rt.registry.Infos(), Tools: []string{}}
	for _, info := range s.rt.opts.ToolInfos {
		if info != nil {
			out.Tools = append(out.Tools, info.Name)
		}
	}
	return out, nil
}

// targetFor resolves the durable target of a new independent input. An
// explicit name must be registered; it never falls back to main.
func (rt *runtime) targetFor(cmd agent.InputCommand) (agent.TargetAgent, error) {
	name := ""
	if cmd.Kind == "prompt" || cmd.Kind == "chat" {
		name = cmd.TargetAgent
	}
	return rt.registry.Target(name, rt.generation)
}

// definitionFor resolves a saved target. A missing or changed definition is
// incompatible; the trace stays browsable and is never re-routed.
func (rt *runtime) definitionFor(target agent.TargetAgent) (agent.AgentDefinition, error) {
	// P1/P2 journals recorded main without a version; they remain main.
	if target.Name == "" || (target.Name == agent.MainAgentName && target.Version == "") {
		target = agent.TargetAgent{Name: agent.MainAgentName, Version: agent.MainAgentVersion}
	}
	return rt.registry.Resolve(target)
}

// targetModel is the model a trace runs with when no explicit selection
// applies: the target's own model, otherwise the session default.
func (rt *runtime) targetModel(trace *state.TraceState) (model.AgenticModel, bool, error) {
	def, err := rt.definitionFor(trace.Target)
	if err != nil {
		return nil, false, err
	}
	if def.Model != nil {
		return def.Model, true, nil
	}
	return nil, false, nil
}

// frameDefinition resolves the saved target of the frame's trace in the mailbox.
func (rt *runtime) frameDefinition(frame *execution) (agent.AgentDefinition, error) {
	value, err := rt.call(context.Background(), func(rt *runtime) (any, error) {
		tr := rt.manager.View().Traces[frame.scope.TraceID]
		if tr == nil {
			return nil, product.NewError(product.CodeNotFound, "trace not found")
		}
		return rt.definitionFor(tr.Target)
	})
	if err != nil {
		return agent.AgentDefinition{}, err
	}
	return value.(agent.AgentDefinition), nil
}

// executable rejects targets this build cannot run before any state changes.
func (rt *runtime) executable(trace *state.TraceState) error {
	_, err := rt.definitionFor(trace.Target)
	return err
}
