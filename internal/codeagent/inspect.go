package codeagent

import (
	"context"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

// SessionDescriptor contains only the immutable journal header binding.
// Inspecting it does not acquire a writer or start a session runtime.
type SessionDescriptor struct {
	SessionID  string
	Workspace  string
	Generation string
}

func InspectSessionHeader(ctx context.Context, stateRoot, sid string, limits config.Limits) (SessionDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return SessionDescriptor{}, err
	}
	header, err := readHeaderWithLimitCode(stateRoot, sid, limits.WithDefaults().MaxCommitLineBytes, product.CodeIncompatibleVersion)
	if err != nil {
		return SessionDescriptor{}, err
	}
	binding, err := decodeBinding(header.Workspace)
	if err != nil {
		return SessionDescriptor{}, err
	}
	if err := ctx.Err(); err != nil {
		return SessionDescriptor{}, err
	}
	return SessionDescriptor{SessionID: header.SessionID, Workspace: binding.HostRealRoot, Generation: binding.Generation}, nil
}

// SubscribeHistoryFrom ends at the durable cursor fixed by the existing replay
// operation. Only a read-only session can use this finite query port.
func SubscribeHistoryFrom(ctx context.Context, session *AgentSession, after uint64, limits config.Limits) (*ReplaySubscription, error) {
	if session == nil || !session.rt.opts.ReadOnly {
		return nil, product.NewError(product.CodeInvalidArgument, "history query requires a read-only session")
	}
	return session.subscribeFrom(ctx, after, limits, true)
}
