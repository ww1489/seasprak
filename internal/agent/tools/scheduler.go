package tools

import (
	"context"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

const maxResourceReaders = 4

// ResourceScheduler is a small process-wide FIFO scheduler. It grants a
// request's complete normalized resource set atomically.
type ResourceScheduler struct {
	mu     sync.Mutex
	queue  []*resourceWaiter
	active map[string]resourceUse
	holds  map[string][]resourceClaim
}

type resourceUse struct {
	readers          int
	writers          int
	transientReaders int
	transientWriters int
}

type resourceClaim struct {
	key     string
	write   bool
	bounded bool
}

type resourceWaiter struct {
	claims []resourceClaim
	ready  chan struct{}
}

type ResourceRequest struct {
	Environment string
	Workspace   string
	Resources   []agent.ExecutionResource
	Effect      string
	Concurrency string
}

type ResourceHold struct {
	ID      string
	Request ResourceRequest
}

type ResourceLease struct {
	once      sync.Once
	scheduler *ResourceScheduler
	claims    []resourceClaim
}

func NewResourceScheduler() *ResourceScheduler {
	return &ResourceScheduler{active: map[string]resourceUse{}, holds: map[string][]resourceClaim{}}
}

func ResourceHoldID(sessionID, callID string) string { return sessionID + "\x00" + callID }

func (s *ResourceScheduler) Acquire(ctx context.Context, request ResourceRequest) (*ResourceLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	claims, err := normalizedResources(request)
	if err != nil {
		return nil, err
	}
	if len(claims) == 0 {
		return &ResourceLease{}, nil
	}
	waiter := &resourceWaiter{claims: claims, ready: make(chan struct{})}
	s.mu.Lock()
	s.queue = append(s.queue, waiter)
	s.grantLocked()
	s.mu.Unlock()

	select {
	case <-waiter.ready:
		lease := &ResourceLease{scheduler: s, claims: claims}
		if err := ctx.Err(); err != nil {
			lease.Release()
			return nil, err
		}
		return lease, nil
	case <-ctx.Done():
		s.mu.Lock()
		removed := false
		for i, queued := range s.queue {
			if queued == waiter {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				removed = true
				break
			}
		}
		if removed {
			s.grantLocked()
		}
		s.mu.Unlock()
		if !removed {
			// The grant won the race. Release it before returning cancellation;
			// a cancelled waiter is never allowed to enter its backend.
			lease := &ResourceLease{scheduler: s, claims: claims}
			lease.Release()
			return nil, ctx.Err()
		}
		return nil, ctx.Err()
	}
}

func (l *ResourceLease) Release() {
	if l == nil || l.scheduler == nil {
		return
	}
	l.once.Do(func() { l.scheduler.releaseClaims(l.claims) })
}

func (l *ResourceLease) Retain(id string) error {
	if l == nil || l.scheduler == nil || id == "" {
		return product.NewError(product.CodeInvalidArgument, "resource hold identity is required")
	}
	retained := false
	l.once.Do(func() {
		s := l.scheduler
		s.mu.Lock()
		defer s.mu.Unlock()
		if prior, ok := s.holds[id]; ok && !sameResourceClaims(prior, l.claims) {
			return
		}
		if _, ok := s.holds[id]; !ok {
			for _, claim := range l.claims {
				use := s.active[claim.key]
				if claim.write {
					if use.transientWriters == 0 {
						return
					}
				} else if use.transientReaders == 0 {
					return
				}
			}
			for _, claim := range l.claims {
				use := s.active[claim.key]
				if claim.write {
					use.transientWriters--
				} else {
					use.transientReaders--
				}
				s.active[claim.key] = use
			}
		}
		s.holds[id] = append([]resourceClaim(nil), l.claims...)
		retained = true
	})
	if !retained {
		return product.NewError(product.CodeStateConflict, "resource lease cannot be retained")
	}
	return nil
}

func (s *ResourceScheduler) RestoreHold(id string, request ResourceRequest) error {
	return s.RestoreHolds([]ResourceHold{{ID: id, Request: request}})
}

func (s *ResourceScheduler) RestoreHolds(holds []ResourceHold) error {
	if s == nil {
		return product.NewError(product.CodeInvalidArgument, "resource scheduler is required")
	}
	type preparedHold struct {
		id     string
		claims []resourceClaim
	}
	prepared := make([]preparedHold, 0, len(holds))
	seen := make(map[string][]resourceClaim, len(holds))
	for _, hold := range holds {
		if hold.ID == "" {
			return product.NewError(product.CodeInvalidArgument, "resource hold identity is required")
		}
		claims, err := normalizedResources(hold.Request)
		if err != nil {
			return err
		}
		if prior, ok := seen[hold.ID]; ok {
			if !sameResourceClaims(prior, claims) {
				return product.NewError(product.CodeStateConflict, "resource hold description changed")
			}
			continue
		}
		seen[hold.ID] = claims
		prepared = append(prepared, preparedHold{id: hold.ID, claims: claims})
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, hold := range prepared {
		if prior, ok := s.holds[hold.id]; ok {
			if !sameResourceClaims(prior, hold.claims) {
				return product.NewError(product.CodeStateConflict, "resource hold description changed")
			}
			continue
		}
		for _, claim := range hold.claims {
			use := s.active[claim.key]
			if use.transientReaders != 0 || use.transientWriters != 0 {
				return product.NewError(product.CodeStateConflict, "resource hold requires reconciliation with an active execution")
			}
		}
	}
	for _, hold := range prepared {
		if _, ok := s.holds[hold.id]; ok || len(hold.claims) == 0 {
			continue
		}
		for _, claim := range hold.claims {
			addActiveClaim(s.active, claim, false)
		}
		s.holds[hold.id] = append([]resourceClaim(nil), hold.claims...)
	}
	return nil
}

func (s *ResourceScheduler) HasHold(id string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.holds[id]
	return ok
}

func (s *ResourceScheduler) ReleaseHold(id string) error {
	if s == nil || id == "" {
		return product.NewError(product.CodeInvalidArgument, "resource hold identity is required")
	}
	s.mu.Lock()
	claims, ok := s.holds[id]
	if ok {
		delete(s.holds, id)
	}
	s.mu.Unlock()
	if ok {
		s.releaseHeldClaims(claims)
	}
	return nil
}

func addActiveClaim(active map[string]resourceUse, claim resourceClaim, transient bool) {
	use := active[claim.key]
	if claim.write {
		use.writers++
		if transient {
			use.transientWriters++
		}
	} else {
		use.readers++
		if transient {
			use.transientReaders++
		}
	}
	active[claim.key] = use
}

func removeActiveClaim(active map[string]resourceUse, claim resourceClaim, transient bool) {
	use := active[claim.key]
	if claim.write {
		if use.writers > 0 {
			use.writers--
		}
		if transient && use.transientWriters > 0 {
			use.transientWriters--
		}
	} else {
		if use.readers > 0 {
			use.readers--
		}
		if transient && use.transientReaders > 0 {
			use.transientReaders--
		}
	}
	if use.readers == 0 && use.writers == 0 {
		delete(active, claim.key)
	} else {
		active[claim.key] = use
	}
}

func (s *ResourceScheduler) releaseClaims(claims []resourceClaim) {
	s.releaseClaimsKind(claims, true)
}

func (s *ResourceScheduler) releaseHeldClaims(claims []resourceClaim) {
	s.releaseClaimsKind(claims, false)
}

func (s *ResourceScheduler) releaseClaimsKind(claims []resourceClaim, transient bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, claim := range claims {
		removeActiveClaim(s.active, claim, transient)
	}
	s.grantLocked()
}

func sameResourceClaims(left, right []resourceClaim) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (s *ResourceScheduler) grantLocked() {
	for len(s.queue) > 0 {
		waiter := s.queue[0]
		if !s.canGrantLocked(waiter) {
			return
		}
		s.queue = s.queue[1:]
		for _, claim := range waiter.claims {
			addActiveClaim(s.active, claim, true)
		}
		close(waiter.ready)
		if waiterHasWrite(waiter) {
			return
		}
		// Consecutive compatible readers may start together, but never bypass
		// the first blocked writer in FIFO order.
	}
}

func (s *ResourceScheduler) canGrantLocked(waiter *resourceWaiter) bool {
	for _, claim := range waiter.claims {
		use := s.active[claim.key]
		if claim.write {
			if use.writers != 0 || use.readers != 0 {
				return false
			}
		} else if use.writers != 0 || (claim.bounded && use.readers >= maxResourceReaders) {
			return false
		}
	}
	return true
}

func waiterHasWrite(waiter *resourceWaiter) bool {
	for _, claim := range waiter.claims {
		if claim.write {
			return true
		}
	}
	return false
}

func normalizedResources(request ResourceRequest) ([]resourceClaim, error) {
	effect := strings.ToLower(request.Effect)
	if effect == "none" || effect == "control" {
		return nil, nil
	}
	environment := strings.TrimSpace(request.Environment)
	workspace := strings.ReplaceAll(strings.TrimSpace(request.Workspace), `\`, "/")
	if environment == "" || workspace == "" {
		return nil, product.NewError(product.CodeInvalidArgument, "resource scheduling domain is required")
	}
	workspace = path.Clean(workspace)
	if runtime.GOOS == "windows" {
		workspace = strings.ToLower(workspace)
	}
	prefix := environment + "\x00" + workspace + "\x00"
	workspaceGuard := prefix + "workspace:guard"
	workspaceReadCapacity := prefix + "workspace:read-capacity"
	writeResources := effect != "read" || strings.EqualFold(request.Concurrency, "exclusive")
	seen := map[string]struct{}{}
	claims := make([]resourceClaim, 0, len(request.Resources)+1)
	for _, resource := range request.Resources {
		identity, err := normalizeResourceIdentity(resource.Identity)
		if err != nil {
			return nil, err
		}
		key := prefix + "resource:" + identity
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		claims = append(claims, resourceClaim{key: key, write: writeResources})
	}
	if len(claims) == 0 {
		// Unknown or undeclared side effects exclude every declared operation in
		// the same real workspace through the shared guard.
		return []resourceClaim{{key: workspaceGuard, write: true}}, nil
	}
	claims = append(claims, resourceClaim{key: workspaceGuard})
	if !writeResources {
		claims = append(claims, resourceClaim{key: workspaceReadCapacity, bounded: true})
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].key < claims[j].key })
	return claims, nil
}

func hasWindowsLogicalRoot(identity string) bool {
	if len(identity) >= 2 {
		first := identity[0]
		if (first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z') && identity[1] == ':' {
			return true
		}
	}
	return strings.HasPrefix(identity, `\\`) || strings.HasPrefix(identity, "//")
}

func normalizeResourceIdentity(identity string) (string, error) {
	trimmed := strings.TrimSpace(identity)
	if trimmed == "" || hasWindowsLogicalRoot(trimmed) {
		return "", product.NewError(product.CodeInvalidArgument, "resource identity must be a nonempty logical relative identity")
	}
	raw := strings.ReplaceAll(trimmed, `\`, "/")
	if strings.HasPrefix(raw, "/") || path.IsAbs(raw) {
		return "", product.NewError(product.CodeInvalidArgument, "resource identity must be a nonempty logical relative identity")
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", product.NewError(product.CodeInvalidArgument, "resource identity cannot traverse its logical root")
		}
	}
	clean := path.Clean(raw)
	if clean == "." || clean == "" || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", product.NewError(product.CodeInvalidArgument, "resource identity is not canonical")
	}
	if runtime.GOOS == "windows" {
		clean = strings.ToLower(clean)
	}
	return clean, nil
}

var sharedResourceScheduler = NewResourceScheduler()

func SharedResourceScheduler() *ResourceScheduler { return sharedResourceScheduler }
