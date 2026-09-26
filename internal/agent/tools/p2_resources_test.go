package tools

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestP2ResourcesRetainedHoldRestoresIdempotentlyAndRejectsDrift(t *testing.T) {
	s := NewResourceScheduler()
	request := ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "dir/file"}}, Effect: "write"}
	lease, err := s.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	id := ResourceHoldID("session", "call")
	if err := lease.Retain(id); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreHold(id, request); err != nil {
		t.Fatalf("idempotent restore: %v", err)
	}
	changed := request
	changed.Resources = []agent.ExecutionResource{{Identity: "other"}}
	if err := s.RestoreHold(id, changed); err == nil {
		t.Fatal("same hold id accepted a different description")
	}
	ctx, cancel := context.WithCancel(t.Context())
	blocked := make(chan error, 1)
	go func() {
		_, err := s.Acquire(ctx, request)
		blocked <- err
	}()
	waitResourceQueue(t, s, 1)
	cancel()
	if err := <-blocked; !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked acquire err=%v", err)
	}
	if err := s.ReleaseHold(id); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseHold(id); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
}

func TestP2ResourcesRestoredWritersReleaseIndependently(t *testing.T) {
	s := NewResourceScheduler()
	request := ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "dir/file"}}, Effect: "write"}
	firstID := ResourceHoldID("session", "first")
	secondID := ResourceHoldID("session", "second")
	if err := s.RestoreHold(firstID, request); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreHold(secondID, request); err != nil {
		t.Fatal(err)
	}

	acquired := make(chan *ResourceLease, 1)
	go func() {
		lease, _ := s.Acquire(t.Context(), request)
		acquired <- lease
	}()
	waitResourceQueue(t, s, 1)
	if err := s.ReleaseHold(firstID); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	queuedAfterFirstRelease := len(s.queue)
	s.mu.Unlock()
	if queuedAfterFirstRelease != 1 {
		t.Fatalf("releasing one restored writer unblocked %d queued requests", 1-queuedAfterFirstRelease)
	}
	if err := s.ReleaseHold(firstID); err != nil {
		t.Fatalf("repeated release changed writer count: %v", err)
	}
	if err := s.ReleaseHold(secondID); err != nil {
		t.Fatal(err)
	}
	lease := <-acquired
	if lease == nil {
		t.Fatal("request was not granted after the last writer released")
	}
	lease.Release()
}

func TestP2ResourcesRetainedLeaseBecomesHistoricalHold(t *testing.T) {
	s := NewResourceScheduler()
	request := ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "dir/file"}}, Effect: "write"}
	lease, err := s.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	firstID := ResourceHoldID("session", "first")
	secondID := ResourceHoldID("session", "second")
	if err := lease.Retain(firstID); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreHold(secondID, request); err != nil {
		t.Fatalf("retained lease still appeared as a transient execution: %v", err)
	}
	if err := s.ReleaseHold(firstID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseHold(secondID); err != nil {
		t.Fatal(err)
	}
}

func TestP2ResourcesRestoreHoldsRejectsTransientConflictsAtomically(t *testing.T) {
	writeRequest := ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "shared"}}, Effect: "write"}
	readRequest := writeRequest
	readRequest.Effect = "read"

	for _, tc := range []struct {
		name    string
		active  ResourceRequest
		restore ResourceRequest
	}{
		{name: "active-writer", active: writeRequest, restore: readRequest},
		{name: "active-reader", active: readRequest, restore: writeRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewResourceScheduler()
			lease, err := s.Acquire(t.Context(), tc.active)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			err = s.RestoreHold(ResourceHoldID("session", tc.name), tc.restore)
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict {
				t.Fatalf("restore against transient use error=%v", err)
			}
			if s.HasHold(ResourceHoldID("session", tc.name)) {
				t.Fatal("failed restore installed a hold")
			}
		})
	}

	s := NewResourceScheduler()
	active, err := s.Acquire(t.Context(), writeRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Release()
	firstID := ResourceHoldID("session", "first")
	secondID := ResourceHoldID("session", "second")
	err = s.RestoreHolds([]ResourceHold{
		{ID: firstID, Request: ResourceRequest{Environment: "memory", Workspace: "other", Effect: "unknown"}},
		{ID: secondID, Request: readRequest},
	})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict {
		t.Fatalf("batch restore error=%v", err)
	}
	if s.HasHold(firstID) || s.HasHold(secondID) {
		t.Fatal("failed batch restore partially installed holds")
	}
}

func TestP2ResourcesIdentityAliasesConflictAndInvalidIdentityFails(t *testing.T) {
	base, err := normalizedResources(ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "dir/file"}}, Effect: "write"})
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"dir/./file", `dir\file`} {
		got, err := normalizedResources(ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: alias}}, Effect: "write"})
		if err != nil {
			t.Fatalf("alias %q rejected: %v", alias, err)
		}
		if !sameResourceClaims(base, got) {
			t.Fatalf("alias %q normalized to different claims: base=%+v got=%+v", alias, base, got)
		}
	}
	s := NewResourceScheduler()
	for _, invalid := range []string{"", ".", "dir/../file", "/absolute"} {
		if _, err := s.Acquire(t.Context(), ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: invalid}}, Effect: "write"}); err == nil {
			t.Fatalf("invalid resource identity %q was accepted", invalid)
		}
	}
}

func TestP2ResourcesRejectsWindowsVolumeAndDeviceIdentitiesOnEveryPlatform(t *testing.T) {
	invalid := []string{
		`C:/x`,
		`C:\x`,
		`C:relative`,
		`//server/share`,
		`\\server\share`,
		`//?/C:/x`,
		`//./device`,
	}
	for _, identity := range invalid {
		t.Run(identity, func(t *testing.T) {
			_, err := normalizedResources(ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: identity}}, Effect: "write"})
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
				t.Fatalf("identity %q error=%v", identity, err)
			}
		})
	}
}

func TestP2ResourcesWindowsCaseAliasesConflict(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows resource identity case folding")
	}
	upper, err := normalizedResources(ResourceRequest{Environment: "memory", Workspace: `C:\Work`, Resources: []agent.ExecutionResource{{Identity: "Dir/File"}}, Effect: "write"})
	if err != nil {
		t.Fatal(err)
	}
	lower, err := normalizedResources(ResourceRequest{Environment: "memory", Workspace: `c:\work`, Resources: []agent.ExecutionResource{{Identity: "dir/file"}}, Effect: "write"})
	if err != nil {
		t.Fatal(err)
	}
	if !sameResourceClaims(upper, lower) {
		t.Fatalf("Windows case aliases differ: upper=%+v lower=%+v", upper, lower)
	}
}

func TestP2ResourcesTwoReadsThenWriter(t *testing.T) {
	s := NewResourceScheduler()
	request := ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "file"}}, Effect: "read"}
	first, err := s.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	writer := make(chan *ResourceLease, 1)
	go func() {
		lease, _ := s.Acquire(t.Context(), ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: request.Resources, Effect: "write"})
		writer <- lease
	}()
	waitResourceQueue(t, s, 1)
	select {
	case <-writer:
		t.Fatal("writer overlapped readers")
	default:
	}
	first.Release()
	select {
	case <-writer:
		t.Fatal("writer started before all readers ended")
	default:
	}
	second.Release()
	lease := <-writer
	if lease == nil {
		t.Fatal("writer did not receive lease")
	}
	lease.Release()
}

func TestP2ResourcesFifthDifferentResourceReadWaitsForWorkspaceCapacity(t *testing.T) {
	s := NewResourceScheduler()
	leases := make([]*ResourceLease, 4)
	for i := range leases {
		var err error
		leases[i], err = s.Acquire(t.Context(), ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: fmt.Sprintf("file-%d", i)}}, Effect: "read"})
		if err != nil {
			t.Fatal(err)
		}
	}
	fifthRequest := ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "file-4"}}, Effect: "read"}
	fifth := make(chan *ResourceLease, 1)
	go func() {
		lease, _ := s.Acquire(t.Context(), fifthRequest)
		fifth <- lease
	}()
	waitResourceQueue(t, s, 1)
	leases[0].Release()
	lease := <-fifth
	if lease == nil {
		t.Fatal("fifth workspace reader did not start after one slot released")
	}
	lease.Release()
	for _, held := range leases[1:] {
		held.Release()
	}
}

func TestP2ResourcesRestoredReadHoldConsumesWorkspaceCapacity(t *testing.T) {
	s := NewResourceScheduler()
	restored := ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "restored"}}, Effect: "read"}
	if err := s.RestoreHold(ResourceHoldID("session", "restored"), restored); err != nil {
		t.Fatal(err)
	}
	leases := make([]*ResourceLease, 3)
	for i := range leases {
		var err error
		leases[i], err = s.Acquire(t.Context(), ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: fmt.Sprintf("live-%d", i)}}, Effect: "read"})
		if err != nil {
			t.Fatal(err)
		}
	}
	blockedRequest := ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "blocked"}}, Effect: "read"}
	blocked := make(chan *ResourceLease, 1)
	go func() {
		lease, _ := s.Acquire(t.Context(), blockedRequest)
		blocked <- lease
	}()
	waitResourceQueue(t, s, 1)
	if err := s.ReleaseHold(ResourceHoldID("session", "restored")); err != nil {
		t.Fatal(err)
	}
	lease := <-blocked
	if lease == nil {
		t.Fatal("restored read hold did not release workspace capacity")
	}
	lease.Release()
	for _, held := range leases {
		held.Release()
	}
}

func TestP2ResourcesRetainedReadKeepsWorkspaceCapacityCount(t *testing.T) {
	s := NewResourceScheduler()
	firstRequest := ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "first"}}, Effect: "read"}
	first, err := s.Acquire(t.Context(), firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	firstID := ResourceHoldID("session", "first")
	if err := first.Retain(firstID); err != nil {
		t.Fatal(err)
	}
	leases := make([]*ResourceLease, 3)
	for i := range leases {
		leases[i], err = s.Acquire(t.Context(), ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: fmt.Sprintf("other-%d", i)}}, Effect: "read"})
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	_, err = s.Acquire(ctx, ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "fifth"}}, Effect: "read"})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retained read lost its workspace slot: %v", err)
	}
	if err := s.ReleaseHold(firstID); err != nil {
		t.Fatal(err)
	}
	lease, err := s.Acquire(t.Context(), ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "fifth"}}, Effect: "read"})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	for _, held := range leases {
		held.Release()
	}
}

func TestP2ResourcesFifthReadWaits(t *testing.T) {
	s := NewResourceScheduler()
	request := ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "file"}}, Effect: "read"}
	leases := make([]*ResourceLease, 4)
	for i := range leases {
		var err error
		leases[i], err = s.Acquire(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
	}
	fifth := make(chan *ResourceLease, 1)
	go func() {
		lease, _ := s.Acquire(t.Context(), request)
		fifth <- lease
	}()
	waitResourceQueue(t, s, 1)
	select {
	case <-fifth:
		t.Fatal("fifth reader exceeded limit")
	default:
	}
	leases[0].Release()
	lease := <-fifth
	if lease == nil {
		t.Fatal("fifth reader did not start after capacity released")
	}
	lease.Release()
	for _, held := range leases[1:] {
		held.Release()
	}
}

func TestP2ResourcesWriterIsNotStarvedByLaterReader(t *testing.T) {
	s := NewResourceScheduler()
	read := ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "file"}}, Effect: "read"}
	active := make([]*ResourceLease, 4)
	for i := range active {
		active[i], _ = s.Acquire(t.Context(), read)
	}
	order := make(chan string, 2)
	leases := make(chan *ResourceLease, 2)
	go func() {
		lease, _ := s.Acquire(t.Context(), ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: read.Resources, Effect: "write"})
		order <- "writer"
		leases <- lease
	}()
	waitResourceQueue(t, s, 1)
	go func() {
		lease, _ := s.Acquire(t.Context(), read)
		order <- "reader"
		leases <- lease
	}()
	waitResourceQueue(t, s, 2)
	for _, held := range active {
		held.Release()
	}
	if got := <-order; got != "writer" {
		t.Fatalf("later reader bypassed writer: %s", got)
	}
	writer := <-leases
	select {
	case got := <-order:
		t.Fatalf("%s overlapped exclusive writer", got)
	default:
	}
	writer.Release()
	if got := <-order; got != "reader" {
		t.Fatalf("reader did not follow writer: %s", got)
	}
	(<-leases).Release()
}

func TestP2ResourcesCompleteSetsAvoidDeadlock(t *testing.T) {
	s := NewResourceScheduler()
	first, err := s.Acquire(t.Context(), ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "a"}, {Identity: "b"}}, Effect: "write"})
	if err != nil {
		t.Fatal(err)
	}
	second := make(chan *ResourceLease, 1)
	go func() {
		lease, _ := s.Acquire(t.Context(), ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "b"}, {Identity: "a"}, {Identity: "a"}}, Effect: "write"})
		second <- lease
	}()
	waitResourceQueue(t, s, 1)
	first.Release()
	lease := <-second
	if lease == nil {
		t.Fatal("complete resource set was not granted")
	}
	lease.Release()
}

func TestP2ResourcesCancelledWaiterDoesNotStartOrBreakQueue(t *testing.T) {
	s := NewResourceScheduler()
	write := ResourceRequest{Environment: "memory", Workspace: "workspace", Effect: "unknown"}
	held, err := s.Acquire(t.Context(), write)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancelled := make(chan error, 1)
	go func() {
		_, err := s.Acquire(ctx, write)
		cancelled <- err
	}()
	waitResourceQueue(t, s, 1)
	cancel()
	if err := <-cancelled; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	next := make(chan *ResourceLease, 1)
	go func() {
		lease, _ := s.Acquire(t.Context(), write)
		next <- lease
	}()
	waitResourceQueue(t, s, 1)
	held.Release()
	lease := <-next
	if lease == nil {
		t.Fatal("cancelled waiter broke queue")
	}
	lease.Release()
}

func TestP2ResourcesUnknownConflictsWithDeclaredWorkspaceUse(t *testing.T) {
	s := NewResourceScheduler()
	declared, err := s.Acquire(t.Context(), ResourceRequest{Environment: "memory", Workspace: "workspace", Resources: []agent.ExecutionResource{{Identity: "file"}}, Effect: "read"})
	if err != nil {
		t.Fatal(err)
	}
	unknown := make(chan *ResourceLease, 1)
	go func() {
		lease, _ := s.Acquire(t.Context(), ResourceRequest{Environment: "memory", Workspace: "workspace", Effect: "unknown"})
		unknown <- lease
	}()
	waitResourceQueue(t, s, 1)
	select {
	case <-unknown:
		t.Fatal("unknown effect overlapped declared workspace use")
	default:
	}
	declared.Release()
	lease := <-unknown
	if lease == nil {
		t.Fatal("unknown effect was not granted after workspace became idle")
	}
	lease.Release()
}

func TestP2ResourcesNoneDoesNotTakeWorkspaceExclusive(t *testing.T) {
	s := NewResourceScheduler()
	write, err := s.Acquire(t.Context(), ResourceRequest{Environment: "memory", Workspace: "workspace", Effect: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	none, err := s.Acquire(t.Context(), ResourceRequest{Environment: "memory", Workspace: "workspace", Effect: "none"})
	if err != nil {
		t.Fatal(err)
	}
	none.Release()
	write.Release()
}

func waitResourceQueue(t *testing.T, s *ResourceScheduler, want int) {
	t.Helper()
	for {
		s.mu.Lock()
		got := len(s.queue)
		s.mu.Unlock()
		if got == want {
			return
		}
		select {
		case <-t.Context().Done():
			t.Fatalf("queue length=%d want=%d", got, want)
		default:
			runtime.Gosched()
		}
	}
}

var _ = sync.Once{}
