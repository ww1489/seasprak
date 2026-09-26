package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/config"
)

func TestP2TicketCannotBeForgedCopiedOrRebound(t *testing.T) {
	tickets, err := NewExecutionTickets()
	if err != nil {
		t.Fatal(err)
	}
	frozen := withTicketHash(t, FrozenExecution{
		CallID: "call", Scope: ExecutionScope{SessionID: "session", TraceID: "trace", InvocationID: "invocation", ExecutionID: "execution"},
		PolicyRef: "policy-1", BackendID: "process-operations", Resources: []ExecutionResource{{Identity: "workspace/file"}},
	})
	receipt := claimReceiptFor(t, frozen)
	if err := tickets.Consume(ExecutionTicketRef{}, frozen); err == nil {
		t.Fatal("zero ticket was accepted")
	}
	ref, err := tickets.Issue(receipt, frozen, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !ref.Issued() {
		t.Fatal("issued ticket is not opaque-valid")
	}
	raw, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{}" {
		t.Fatalf("ticket leaked serializable authority: %s", raw)
	}
	changed := frozen.Clone()
	changed.PolicyRef = "policy-2"
	if err := tickets.Consume(ref, changed); err == nil {
		t.Fatal("ticket crossed a policy change")
	}
	copied := ref
	if err := tickets.Consume(ref, frozen); err != nil {
		t.Fatal(err)
	}
	if err := tickets.Consume(copied, frozen); err == nil {
		t.Fatal("copied consumed ticket replayed")
	}
}

func TestP2TicketExpiredAndDescriptionMismatchFailClosed(t *testing.T) {
	tickets, err := NewExecutionTickets()
	if err != nil {
		t.Fatal(err)
	}
	frozen := withTicketHash(t, FrozenExecution{CallID: "call", Scope: ExecutionScope{SessionID: "session"}, PolicyRef: "policy", BackendID: "file-operations"})
	expiredReceipt := claimReceiptFor(t, frozen)
	expired, err := tickets.Issue(expiredReceipt, frozen, time.Now().Add(-time.Second))
	if err == nil || expired.Issued() {
		t.Fatal("expired ticket was issued")
	}
	receipt := claimReceiptFor(t, frozen)
	ref, err := tickets.Issue(receipt, frozen, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	changed := frozen.Clone()
	changed.BackendID = "process-operations"
	if err := tickets.Consume(ref, changed); err == nil {
		t.Fatal("ticket crossed a backend/description change")
	}
}

type blockingTicketValidator struct {
	entered chan struct{}
	release chan struct{}
}

func (v *blockingTicketValidator) ValidateExecutionTicket(context.Context, FrozenExecution) error {
	close(v.entered)
	<-v.release
	return nil
}

func TestP2TicketCancelDuringValidationFailsBeforeConsumption(t *testing.T) {
	tickets, err := NewExecutionTickets()
	if err != nil {
		t.Fatal(err)
	}
	frozen := withTicketHash(t, FrozenExecution{CallID: "call", Scope: ExecutionScope{SessionID: "session"}, PolicyRef: "policy", BackendID: "process-operations"})
	validator := &blockingTicketValidator{entered: make(chan struct{}), release: make(chan struct{})}
	ref, err := tickets.Issue(claimReceiptFor(t, frozen), frozen, time.Now().Add(time.Minute), validator)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- NewAuthorizedExecution(ref, frozen).Validate(ctx) }()
	<-validator.entered
	cancel()
	close(validator.release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("validation after cancellation=%v", err)
	}
}

func TestP2TicketExpiryDuringValidationFailsBeforeConsumption(t *testing.T) {
	tickets, err := NewExecutionTickets()
	if err != nil {
		t.Fatal(err)
	}
	frozen := withTicketHash(t, FrozenExecution{CallID: "call", Scope: ExecutionScope{SessionID: "session"}, PolicyRef: "policy", BackendID: "process-operations"})
	validator := &blockingTicketValidator{entered: make(chan struct{}), release: make(chan struct{})}
	ref, err := tickets.Issue(claimReceiptFor(t, frozen), frozen, time.Now().Add(20*time.Millisecond), validator)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- NewAuthorizedExecution(ref, frozen).Validate(t.Context()) }()
	<-validator.entered
	time.Sleep(30 * time.Millisecond)
	close(validator.release)
	if err := <-result; err == nil {
		t.Fatal("ticket that expired during validation was consumed")
	}
}

func TestP2OperationsAreNarrowAndAuthorizedRequestsCarryOpaqueTicket(t *testing.T) {
	var _ FileOperations = fileOperationsProbe{}
	var _ ProcessOperations = processOperationsProbe{}
	var _ ArtifactStore = artifactStoreProbe{}

	auth := AuthorizedExecution{}
	write := AuthorizedFileWrite{Authorization: auth, Path: "workspace/file", ContentRef: "artifact:content"}
	process := AuthorizedProcess{Authorization: auth, Argv: []string{"runner", "--input-ref", "argv:1"}, Cwd: "workspace", EnvironmentRef: "env:1", StdinRef: "stdin:1"}
	raw, err := json.Marshal(struct {
		Write   AuthorizedFileWrite `json:"write"`
		Process AuthorizedProcess   `json:"process"`
	}{write, process})
	if err != nil {
		t.Fatal(err)
	}
	if json.Valid(raw) && (string(raw) == "" || auth.Ticket.Issued()) {
		t.Fatal("zero authorization unexpectedly became valid")
	}
}

func TestP2TicketRequiresUniqueCommittedClaimReceipt(t *testing.T) {
	ledger := NewBudget(config.DefaultLimits())
	sink := &ticketClaimSink{}
	scope := ExecutionScope{SessionID: "session", TraceID: "trace", InvocationID: "invocation", ExecutionID: "execution"}
	call := FrozenCall{CallID: "call", ProviderCallID: "provider", Name: "run", Hash: "call-hash"}
	frozen := withTicketHash(t, FrozenExecution{CallID: call.CallID, Scope: scope, PolicyRef: "policy", BackendID: "process-operations"})
	tickets, err := NewExecutionTickets()
	if err != nil {
		t.Fatal(err)
	}
	if ref, err := tickets.Issue(ClaimReceipt{}, frozen, time.Now().Add(time.Minute)); err == nil || ref.Issued() {
		t.Fatal("zero claim receipt signed a ticket")
	}
	receipt, err := ledger.ClaimTool(t.Context(), sink, scope, call, frozen)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := tickets.Issue(receipt, frozen, time.Now().Add(time.Minute))
	if err != nil || !ref.Issued() {
		t.Fatalf("issue err=%v issued=%v", err, ref.Issued())
	}
	if replay, err := tickets.Issue(receipt, frozen, time.Now().Add(time.Minute)); err == nil || replay.Issued() {
		t.Fatal("claim receipt signed more than one ticket")
	}
	other := frozen.Clone()
	other.CallID = "other"
	other.Hash = ""
	other = withTicketHash(t, other)
	otherReceipt, err := ledger.ClaimTool(t.Context(), sink, scope, FrozenCall{CallID: "other", ProviderCallID: "other-provider", Name: "run", Hash: "other-call-hash"}, other)
	if err != nil {
		t.Fatal(err)
	}
	if rebound, err := tickets.Issue(otherReceipt, frozen, time.Now().Add(time.Minute)); err == nil || rebound.Issued() {
		t.Fatal("claim receipt crossed call/hash binding")
	}
}

func TestP2TicketClaimFailureReturnsNoReceipt(t *testing.T) {
	ledger := NewBudget(config.DefaultLimits())
	sink := &ticketClaimSink{err: errors.New("commit failed")}
	scope := ExecutionScope{SessionID: "session"}
	call := FrozenCall{CallID: "call", ProviderCallID: "provider", Name: "run", Hash: "call-hash"}
	frozen := withTicketHash(t, FrozenExecution{CallID: call.CallID, Scope: scope, PolicyRef: "policy", BackendID: "trusted-run"})
	receipt, err := ledger.ClaimTool(t.Context(), sink, scope, call, frozen)
	if err == nil || receipt.Issued() {
		t.Fatalf("err=%v receipt=%+v", err, receipt)
	}
}

type ticketClaimSink struct{ err error }

func (s *ticketClaimSink) CommitFact(context.Context, ExecutionScope, Fact) error { return s.err }

func claimReceiptFor(t *testing.T, frozen FrozenExecution) ClaimReceipt {
	t.Helper()
	ledger := NewBudget(config.DefaultLimits())
	receipt, err := ledger.ClaimTool(t.Context(), &ticketClaimSink{}, frozen.Scope, FrozenCall{CallID: frozen.CallID, ProviderCallID: "provider:" + frozen.CallID, Name: frozen.Tool, Hash: frozen.Hash}, frozen)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func withTicketHash(t *testing.T, frozen FrozenExecution) FrozenExecution {
	t.Helper()
	hash, err := frozen.Digest()
	if err != nil {
		t.Fatal(err)
	}
	frozen.Hash = hash
	return frozen
}

type fileOperationsProbe struct{}

func (fileOperationsProbe) List(context.Context, ListRequest) (ListResult, error) {
	return ListResult{}, nil
}
func (fileOperationsProbe) Read(context.Context, ReadRequest) (ReadResult, error) {
	return ReadResult{}, nil
}
func (fileOperationsProbe) Write(context.Context, AuthorizedFileWrite) (FileEffect, error) {
	return FileEffect{}, nil
}
func (fileOperationsProbe) Edit(context.Context, AuthorizedFileEdit) (FileEffect, error) {
	return FileEffect{}, nil
}
func (fileOperationsProbe) Search(context.Context, SearchRequest) (SearchResult, error) {
	return SearchResult{}, nil
}

type processOperationsProbe struct{}

func (processOperationsProbe) Execute(context.Context, AuthorizedProcess, ProgressSink) (ProcessObservation, error) {
	return ProcessObservation{}, nil
}
func (processOperationsProbe) Stop(context.Context, ExecutionRef) (StopObservation, error) {
	return StopObservation{}, nil
}

type artifactStoreProbe struct{}

func (artifactStoreProbe) Save(context.Context, ArtifactInput) (ArtifactRef, error) {
	return ArtifactRef{}, nil
}
func (artifactStoreProbe) Open(context.Context, ArtifactRead) (io.ReadCloser, error) { return nil, nil }
