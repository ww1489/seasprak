package sessions

import (
	"context"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

type AgentSession struct{ rt *runtime }

func Start(opts Options, manager *state.Manager, generation string) (*AgentSession, error) {
	if !opts.ReadOnly {
		if opts.Profile == ProfileDefault {
			return nil, product.NewError(product.CodeResourceUnavailable, "default file and process backends are not available")
		}
		if opts.Profile != ProfileMemory {
			return nil, product.NewError(product.CodeUnsupportedCapability, "unknown capability profile")
		}
		if opts.Model == nil {
			return nil, product.NewError(product.CodeInvalidArgument, "model is required")
		}
	}
	opts.Limits = opts.Limits.WithDefaults()
	opts.Tools = append([]tools.Definition(nil), opts.Tools...)
	for i := range opts.Tools {
		opts.Tools[i].Schema = append([]byte(nil), opts.Tools[i].Schema...)
	}
	if opts.Store == nil || manager == nil {
		return nil, product.NewError(product.CodeInvalidArgument, "session store and manager are required")
	}
	rt := &runtime{opts: opts, manager: manager, mailbox: make(chan command, 64), done: make(chan struct{}), subs: map[int]*subscription{}, generation: generation}
	view := manager.View()
	// Opening never replays an execution or silently releases an old queue.
	if !opts.ReadOnly && !view.RepairRequired {
		for id, tr := range view.Traces {
			if tr.Started && !terminal(tr.State) && tr.State != "paused" {
				if err := manager.SetTraceState(context.Background(), id, "paused", false); err != nil {
					return nil, err
				}
			}
			if tr.State == "queued" && !tr.Hold {
				if err := manager.HoldIndependent(context.Background(), id); err != nil {
					return nil, err
				}
			}
		}
	}
	rt.cursor = manager.View().Cursor
	go rt.loop()
	return &AgentSession{rt: rt}, nil
}
func (s *AgentSession) SubmitInput(ctx context.Context, cmd agent.InputCommand) (agent.InputReceipt, error) {
	cmd.Content = append([]byte(nil), cmd.Content...)
	if cmd.Principal == "" {
		cmd.Principal = s.rt.opts.Principal
	}
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		target := agent.TargetAgent{Name: "main", Version: "main-v1", Generation: rt.generation}
		if cmd.TargetAgent != "" && (cmd.Kind == "prompt" || cmd.Kind == "chat") {
			target.Name = cmd.TargetAgent
		}
		before := rt.manager.View().LastSeq
		receipt, err := rt.manager.AcceptWithLimits(ctx, cmd, target, rt.opts.Limits)
		if err != nil {
			return nil, err
		}
		// A replayed acceptance receipt never schedules work again.
		if receipt.AcceptedCommit > before {
			rt.schedule()
		}
		return receipt, nil
	})
	if err != nil {
		return agent.InputReceipt{}, err
	}
	return value.(agent.InputReceipt), nil
}
func (s *AgentSession) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	v := s.rt.manager.View()
	return Snapshot{SessionID: s.rt.opts.SessionID, Cursor: v.Cursor, ActiveTrace: v.ActiveTrace, Traces: v.Traces, Inputs: v.Inputs, Messages: v.Messages, Turns: v.Turns, Calls: v.Calls, RepairRequired: v.RepairRequired}, nil
}
