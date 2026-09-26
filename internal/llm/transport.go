package llm

import (
	"context"
	"net/http"
	"sync/atomic"
)

// RequestIdentity is supplied by the execution layer before starting an attempt.
// It contains no credentials, URL query strings or raw provider payloads.
type RequestIdentity struct{ ModelCallID, AttemptID, Purpose string }
type TransportRequest struct {
	RequestIdentity
	TransportAttempt uint64
}

// RequestObserver must commit occupancy before returning nil. Implementations
// share the logical-call budget across attempts; this transport owns no budget.
type RequestObserver interface {
	BeforeRequest(context.Context, TransportRequest) error
}
type requestObservation struct {
	identity RequestIdentity
	observer RequestObserver
	count    *atomic.Uint64
}
type requestObservationKey struct{}

func WithRequestObservation(ctx context.Context, identity RequestIdentity, observer RequestObserver) context.Context {
	return context.WithValue(ctx, requestObservationKey{}, &requestObservation{identity: identity, observer: observer, count: new(atomic.Uint64)})
}

// WithRequestPurpose keeps the current attempt identity and physical sequence
// while accounting for auxiliary cache requests in the same logical budget.
func WithRequestPurpose(ctx context.Context, purpose string) context.Context {
	observation, ok := ctx.Value(requestObservationKey{}).(*requestObservation)
	if !ok {
		return ctx
	}
	identity := observation.identity
	identity.Purpose = purpose
	return context.WithValue(ctx, requestObservationKey{}, &requestObservation{identity: identity, observer: observation.observer, count: observation.count})
}

// UsageCollection enables bounded supplementary parsing for a protocol. It does
// not replace the provider SDK's body consumer or response normalization.
type UsageCollection struct {
	Protocol string
	MaxBytes int
}
type UsageObserver interface {
	ObserveUsage(context.Context, TransportRequest, UsageSnapshot)
}
type usageObservationKey struct{}

func WithUsageObservation(ctx context.Context, observer UsageObserver) context.Context {
	return context.WithValue(ctx, usageObservationKey{}, observer)
}

type observedTransport struct {
	base  http.RoundTripper
	usage UsageCollection
}

func NewObservedTransport(base http.RoundTripper, usage ...UsageCollection) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	transport := &observedTransport{base: base}
	if len(usage) > 0 {
		transport.usage = usage[0]
	}
	return transport
}
func (t *observedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx := r.Context()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	observation, ok := ctx.Value(requestObservationKey{}).(*requestObservation)
	if !ok || observation.observer == nil || observation.identity.ModelCallID == "" || observation.identity.AttemptID == "" || observation.identity.Purpose == "" {
		return nil, invalid("request observation identity and observer are required")
	}
	request := TransportRequest{RequestIdentity: observation.identity, TransportAttempt: observation.count.Add(1)}
	if err := observation.observer.BeforeRequest(ctx, request); err != nil {
		return nil, err
	}
	// A commit may complete after caller cancellation. Occupancy remains consumed,
	// but no request may start after cancellation has been observed.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	response, err := t.base.RoundTrip(r)
	status := 0
	var header http.Header
	if response != nil {
		status, header = response.StatusCode, response.Header
	}
	noteHTTPFailure(ctx, status, header, err)
	if response != nil && response.Body != nil {
		response.Body = &failureObservedBody{ReadCloser: response.Body, ctx: ctx, status: status}
	}
	if err == nil && response != nil && response.Body != nil && t.usage.Protocol != "" {
		collector := NewUsageCollector(t.usage.Protocol, t.usage.MaxBytes)
		if chat, ok := ctx.Value(chatCollectorKey{}).(*UsageCollector); ok && t.usage.Protocol == "openai-chat" {
			collector = chat
		}
		body := WrapUsageBody(response.Body, response.Header.Get("Content-Type"), collector).(*usageReadCloser)
		if observer, ok := ctx.Value(usageObservationKey{}).(UsageObserver); ok && observer != nil {
			body.notify = func(snapshot UsageSnapshot) { observer.ObserveUsage(ctx, request, snapshot) }
		}
		response.Body = body
		if collector.chat != nil {
			closedBody := &chatResponseBody{ReadCloser: body, collector: collector}
			collector.mu.Lock()
			collector.chat.body = closedBody
			collector.mu.Unlock()
			response.Body = closedBody
		}
	}
	return response, err
}
