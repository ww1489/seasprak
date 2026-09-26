package llm

import (
	"context"
	"errors"
	"sync"

	product "github.com/ww1489/seasprak/internal/errors"
)

// CredentialResolver is supplied by trusted application code. It must not load
// credentials implicitly from workspace files. Resolve is called per request.
type CredentialResolver interface {
	Resolve(context.Context, string) (ResolvedCredential, error)
}

// ResolvedCredential is transient authentication, never a configuration field.
// Provider and Endpoint bind the secret to its intended service. AccountScope
// must be a non-secret identity assigned by the trusted resolver, not a token.
type ResolvedCredential struct {
	Secret       string `json:"-"`
	AccountScope string
	Provider     string
	Endpoint     string
}

func (ResolvedCredential) String() string   { return "[redacted credential]" }
func (ResolvedCredential) GoString() string { return "[redacted credential]" }

type credentialContextKey struct{}
type credentialLease struct {
	mu     sync.RWMutex
	value  ResolvedCredential
	active bool
}

// RequestCredential is available only during factory construction and request
// initiation. A protocol adapter must snapshot authentication for its one request
// (including the entire stream), never retain this context in a shared client.
// The catalog does not retain the returned secret or resolve it during Recv.
func RequestCredential(ctx context.Context) (ResolvedCredential, bool) {
	lease, ok := ctx.Value(credentialContextKey{}).(*credentialLease)
	if !ok {
		return ResolvedCredential{}, false
	}
	lease.mu.RLock()
	defer lease.mu.RUnlock()
	return lease.value, lease.active
}

func requestContext(ctx context.Context, config ModelConfig, resolver CredentialResolver) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if config.NoCredentials {
		return ctx, func() {}, nil
	}
	denied := func() (context.Context, func(), error) {
		return nil, nil, product.NewError(product.CodeUnauthenticated, "model credential is unavailable or has changed scope")
	}
	if resolver == nil {
		return denied()
	}
	auth, err := resolver.Resolve(ctx, config.CredentialRef)
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if err != nil || auth.Secret == "" || auth.AccountScope != config.AccountScope || auth.Provider != config.Provider || auth.Endpoint != config.Endpoint {
		return denied()
	}
	lease := &credentialLease{value: auth, active: true}
	release := func() { lease.mu.Lock(); lease.value = ResolvedCredential{}; lease.active = false; lease.mu.Unlock() }
	return context.WithValue(ctx, credentialContextKey{}, lease), release, nil
}

// Raw errors may contain credential values, endpoint query strings or bodies.
// Trusted classification originates only at the observed transport boundary.
func safeModelError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if code := vetoCode(err); code != "" {
		return product.NewError(code, "model request rejected")
	}
	if info, ok := ModelFailure(err); ok {
		return classified(info)
	}
	return product.NewError(product.CodeResourceUnavailable, "model request failed")
}
