package eino

import (
	"context"
	"sync"

	"github.com/ww1489/seasprak/agent"
)

type scopeBox struct {
	mu    sync.Mutex
	scope agent.ExecutionScope
	ok    bool
}

type scopeKey struct{}
type turnKey struct{}

// WithExecutionScope stores a mutable scope in ctx. Later turn updates are visible
// to tools and hooks that still hold this context.
func WithExecutionScope(ctx context.Context, scope agent.ExecutionScope) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, scopeKey{}, &scopeBox{scope: scope, ok: true})
}

// ScopeFromContext returns the scope carried by ctx, or fallback when ctx has none.
func ScopeFromContext(ctx context.Context, fallback agent.ExecutionScope) agent.ExecutionScope {
	if ctx == nil {
		return fallback
	}
	box, _ := ctx.Value(scopeKey{}).(*scopeBox)
	if box == nil {
		return fallback
	}
	box.mu.Lock()
	defer box.mu.Unlock()
	if !box.ok {
		return fallback
	}
	return box.scope
}

func noteScope(ctx context.Context, scope agent.ExecutionScope) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if box, _ := ctx.Value(scopeKey{}).(*scopeBox); box != nil {
		box.mu.Lock()
		box.scope = scope
		box.ok = true
		box.mu.Unlock()
		return ctx
	}
	return WithExecutionScope(ctx, scope)
}

func markTurn(ctx context.Context) context.Context {
	return context.WithValue(ctx, turnKey{}, true)
}

func turnStarted(ctx context.Context) bool {
	started, _ := ctx.Value(turnKey{}).(bool)
	return started
}
