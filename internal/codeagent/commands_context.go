package codeagent

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
)

// flushHostCommands runs in the existing mailbox. The manager independently
// protects every nonterminal started Trace, not only rt.active, so paused
// checkpoints and same-Trace continuations cannot gain unrelated history.
func (rt *runtime) flushHostCommands(ctx context.Context) error {
	if rt.active != nil || rt.opts.ReadOnly {
		return nil
	}
	return rt.manager.ConsumeHostCommands(ctx)
}

// snapshotMessages orders display by original durable finalization, not the
// later model-context consumption. The immutable command appears exactly once.
// v must be a disposable, exclusively owned Manager.View copy, with private
// resume validation already complete; projecting consumes its message objects.
func snapshotMessages(v state.View) []agent.AgentMessage {
	var out []agent.AgentMessage
	seen := make(map[string]bool)
	for _, msg := range v.Messages {
		if _, host := v.HostCommands[msg.ID]; !host {
			out = append(out, agent.PublicMessageOwned(msg))
			seen[msg.ID] = true
		}
	}
	for id, result := range v.HostCommands {
		if !seen[id] {
			out = append(out, agent.PublicMessageOwned(result.Message))
			seen[id] = true
		}
	}
	if len(v.HostCommands) == 0 {
		return out
	}
	order := make(map[string]int)
	for i, ev := range v.Events {
		if ev.Type != "message.finalized" {
			continue
		}
		var identity struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(ev.Payload, &identity) == nil {
			if _, exists := order[identity.ID]; !exists {
				order[identity.ID] = i + 1
			}
		}
	}
	// Legacy messages without events retain their original relative order.
	for i, msg := range out {
		if order[msg.ID] == 0 {
			order[msg.ID] = len(v.Events) + i + 1
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return order[out[i].ID] < order[out[j].ID] })
	return out
}
