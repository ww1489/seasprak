package state

import (
	"context"
	"encoding/json"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

func (m *Manager) Accept(ctx context.Context, cmd agent.InputCommand, target agent.TargetAgent) (agent.InputReceipt, error) {
	return m.AcceptWithLimits(ctx, cmd, target, config.DefaultLimits())
}
func (m *Manager) AcceptWithLimits(ctx context.Context, cmd agent.InputCommand, target agent.TargetAgent, limits config.Limits) (agent.InputReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	digest, err := digestCommand(cmd)
	if err != nil {
		return agent.InputReceipt{}, err
	}
	keyBytes, _ := json.Marshal([]string{cmd.Principal, "input", cmd.IdempotencyKey})
	key := string(keyBytes)
	if cmd.IdempotencyKey != "" {
		if old, ok := m.view.Idem[key]; ok {
			if old.Digest != digest {
				return agent.InputReceipt{}, product.NewError(product.CodeIdempotencyConflict, "key already belongs to different content")
			}
			return old.Receipt, nil
		}
	}
	kind, traceID, err := m.classify(cmd, target)
	if err != nil {
		return agent.InputReceipt{}, err
	}
	var trace TraceState
	if traceID == "" {
		traceID = agent.MustID()
		trace = TraceState{ID: traceID, State: "queued", Kind: kind, Target: target, Generation: target.Generation, Limits: limits.WithDefaults()}
	} else {
		trace = *m.view.Traces[traceID]
		target = trace.Target
	}
	id := agent.MustID()
	seq := m.view.LastSeq + 1
	input := InputState{ID: id, TraceID: traceID, Kind: kind, State: "pending", Content: append(json.RawMessage(nil), cmd.Content...), Principal: cmd.Principal, Target: target, CommitSeq: seq}
	receipt := agent.InputReceipt{InputID: id, TraceID: traceID, ActualKind: kind, TargetAgent: target, State: "pending", AcceptedCommit: seq}
	controls := []store.Record{record("input", id, input)}
	if m.view.Traces[traceID] == nil {
		controls = append(controls, record("trace", traceID, trace))
	}
	if cmd.IdempotencyKey != "" {
		controls = append(controls, record("idempotency", key, idemRecord{Digest: digest, Receipt: receipt}))
	}
	_, err = m.commit(ctx, controls, nil, []agent.Event{m.event("input.accepted", traceID, "", receipt)})
	return receipt, err
}
func (m *Manager) classify(cmd agent.InputCommand, target agent.TargetAgent) (string, string, error) {
	if len(cmd.Content) == 0 || !json.Valid(cmd.Content) {
		return "", "", product.NewError(product.CodeInvalidArgument, "valid JSON content is required")
	}
	switch cmd.Kind {
	case "prompt", "chat":
		if cmd.TargetTraceID != "" {
			return "", "", product.NewError(product.CodeInvalidArgument, "independent input cannot target a trace")
		}
		if target.Name != "main" {
			return "", "", product.NewError(product.CodeUnsupportedCapability, "agent is not registered")
		}
		if m.view.HasUnresolvedEffects() {
			return "", "", product.NewError(product.CodeReconciliationRequired, "unresolved tool effects block new work")
		}
		if active := m.view.Traces[m.view.ActiveTrace]; active != nil && active.State == "paused" {
			return "", "", product.NewError(product.CodeReconciliationRequired, "interrupted execution blocks new work")
		}
		if cmd.Kind == "chat" && m.view.ActiveTrace != "" {
			tr := m.view.Traces[m.view.ActiveTrace]
			if tr.State != "running" {
				return "", "", product.NewError(product.CodeStateConflict, "active trace cannot accept chat")
			}
			return "follow_up", tr.ID, nil
		}
		return "prompt", "", nil
	case "steering", "follow_up":
		tr := m.view.Traces[cmd.TargetTraceID]
		if cmd.TargetTraceID == "" {
			return "", "", product.NewError(product.CodeInvalidArgument, "targetTraceId is required")
		}
		if tr == nil {
			return "", "", product.NewError(product.CodeNotFound, "trace not found")
		}
		if terminal(tr.State) || tr.State == "cancelling" {
			return "", "", product.NewError(product.CodeStateConflict, "trace cannot accept directed input")
		}
		if cmd.TargetAgent != "" && cmd.TargetAgent != tr.Target.Name {
			return "", "", product.NewError(product.CodeStateConflict, "target mismatch")
		}
		if m.view.HasUnresolvedEffects() {
			return "", "", product.NewError(product.CodeReconciliationRequired, "unresolved tool effects block new work")
		}
		if active := m.view.Traces[m.view.ActiveTrace]; active != nil && active.State == "paused" {
			return "", "", product.NewError(product.CodeReconciliationRequired, "interrupted execution blocks new work")
		}
		if cmd.Kind == "steering" && tr.State != "running" {
			return "", "", product.NewError(product.CodeStateConflict, "trace cannot accept directed input")
		}
		return cmd.Kind, tr.ID, nil
	}
	return "", "", product.NewError(product.CodeInvalidArgument, "unknown input kind")
}
func (m *Manager) Consume(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	in := m.view.Inputs[id]
	if in == nil {
		return product.NewError(product.CodeNotFound, "input not found")
	}
	if in.State == "consumed" {
		return nil
	}
	if in.State != "pending" {
		return product.NewError(product.CodeStateConflict, "input is not pending")
	}
	next := *in
	next.State = "consumed"
	text := string(in.Content)
	var body struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(in.Content, &body) == nil && body.Text != "" {
		text = body.Text
	}
	msg := agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindUser, Status: agent.StatusComplete, Scope: agent.MessageScope{SessionID: m.sessionID, TraceID: in.TraceID, InputID: id}, Source: agent.SourceRef{Kind: agent.SourceHuman}, Standard: schema.UserAgenticMessage(text)}
	entry := record("message", msg.ID, msg)
	entry.ParentID = m.view.LeafID
	_, err := m.commit(ctx, []store.Record{record("input", id, next)}, []store.Record{entry}, []agent.Event{m.event("input.consumed", in.TraceID, "", next), m.event("message.finalized", in.TraceID, "", msg)})
	return err
}
