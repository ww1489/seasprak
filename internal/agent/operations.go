package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"sync/atomic"
	"time"

	product "github.com/ww1489/seasprak/internal/errors"
)

// ExecutionTicketRef is an opaque, process-local, one-use authority. Its
// fields are deliberately unavailable to callers and omitted from JSON.
type ExecutionTicketRef struct {
	ticket *executionTicket
}

func (r ExecutionTicketRef) Issued() bool { return r.ticket != nil }

type executionTicket struct {
	owner     *ExecutionTickets
	nonce     string
	frozen    FrozenExecution
	expiresAt time.Time
	validator ExecutionTicketValidator
	used      atomic.Bool
}

// ExecutionTicketValidator rechecks live trusted state immediately before a
// backend starts. Session implementations perform this check in their mailbox.
type ExecutionTicketValidator interface {
	ValidateExecutionTicket(context.Context, FrozenExecution) error
}

// ExecutionTickets owns short-lived authorities for one executor. Tickets are
// never persisted and cannot be reconstructed from serialized values.
type ExecutionTickets struct{}

func NewExecutionTickets() (*ExecutionTickets, error) { return &ExecutionTickets{}, nil }

func (t *ExecutionTickets) Issue(receipt ClaimReceipt, frozen FrozenExecution, expiresAt time.Time, validators ...ExecutionTicketValidator) (ExecutionTicketRef, error) {
	if t == nil || frozen.Hash == "" || frozen.CallID == "" || frozen.PolicyRef == "" || frozen.BackendID == "" {
		return ExecutionTicketRef{}, product.NewError(product.CodePermissionDenied, "execution ticket binding is incomplete")
	}
	if !expiresAt.After(time.Now()) {
		return ExecutionTicketRef{}, product.NewError(product.CodePermissionDenied, "execution ticket is expired")
	}
	digest, err := frozen.Digest()
	if err != nil || digest != frozen.Hash {
		return ExecutionTicketRef{}, product.NewError(product.CodePermissionDenied, "execution ticket description is invalid")
	}
	if !receipt.consume(frozen.CallID, frozen.Scope, frozen.Hash) {
		return ExecutionTicketRef{}, product.NewError(product.CodePermissionDenied, "execution ticket requires a committed tool claim")
	}
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return ExecutionTicketRef{}, product.NewError(product.CodeResourceUnavailable, "execution ticket entropy is unavailable")
	}
	var validator ExecutionTicketValidator
	if len(validators) > 0 {
		validator = validators[0]
	}
	ticket := &executionTicket{owner: t, nonce: hex.EncodeToString(nonceBytes), frozen: frozen.Clone(), expiresAt: expiresAt, validator: validator}
	return ExecutionTicketRef{ticket: ticket}, nil
}

func (t *ExecutionTickets) Consume(ref ExecutionTicketRef, frozen FrozenExecution) error {
	return t.consume(context.Background(), ref, frozen)
}

func (t *ExecutionTickets) consume(ctx context.Context, ref ExecutionTicketRef, frozen FrozenExecution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ticket := ref.ticket
	if t == nil || ticket == nil || ticket.owner != t || ticket.nonce == "" {
		return product.NewError(product.CodePermissionDenied, "execution ticket is invalid or already consumed")
	}
	if !time.Now().Before(ticket.expiresAt) || !sameFrozenExecution(ticket.frozen, frozen) {
		return product.NewError(product.CodePermissionDenied, "execution ticket no longer matches the execution")
	}
	if ticket.validator != nil {
		if err := ticket.validator.ValidateExecutionTicket(ctx, frozen.Clone()); err != nil {
			return err
		}
	}
	// Validation may wait on a coordinator mailbox. Recheck cancellation and
	// expiry immediately before the one-use CAS; no blocking work follows it.
	if err := ctx.Err(); err != nil {
		return err
	}
	if !time.Now().Before(ticket.expiresAt) {
		return product.NewError(product.CodePermissionDenied, "execution ticket is expired")
	}
	if !ticket.used.CompareAndSwap(false, true) {
		return product.NewError(product.CodePermissionDenied, "execution ticket is invalid or already consumed")
	}
	return nil
}

func sameFrozenExecution(left, right FrozenExecution) bool {
	if left.Hash == "" || right.Hash == "" || left.Hash != right.Hash {
		return false
	}
	leftDigest, leftErr := left.Digest()
	rightDigest, rightErr := right.Digest()
	return leftErr == nil && rightErr == nil && leftDigest == left.Hash && rightDigest == right.Hash && leftDigest == rightDigest
}

// AuthorizedExecution carries only an opaque authority and the immutable
// descriptor it is bound to. It intentionally has no JSON authority field.
type AuthorizedExecution struct {
	Ticket ExecutionTicketRef `json:"-"`
	Frozen FrozenExecution    `json:"-"`
}

func NewAuthorizedExecution(ticket ExecutionTicketRef, frozen FrozenExecution) AuthorizedExecution {
	return AuthorizedExecution{Ticket: ticket, Frozen: frozen.Clone()}
}

func (a AuthorizedExecution) Validate(ctx context.Context) error {
	if a.Ticket.ticket == nil || a.Ticket.ticket.owner == nil {
		return product.NewError(product.CodePermissionDenied, "execution authorization is missing")
	}
	return a.Ticket.ticket.owner.consume(ctx, a.Ticket, a.Frozen)
}

// FileOperations is intentionally split by operation and accepts only resolved
// resource identities. Secret values remain references.
type FileOperations interface {
	List(context.Context, ListRequest) (ListResult, error)
	Read(context.Context, ReadRequest) (ReadResult, error)
	Write(context.Context, AuthorizedFileWrite) (FileEffect, error)
	Edit(context.Context, AuthorizedFileEdit) (FileEffect, error)
	Search(context.Context, SearchRequest) (SearchResult, error)
}

type ListRequest struct{ Root, Cursor string }
type ListResult struct {
	Entries    []FileEntry
	NextCursor string
}
type FileEntry struct {
	Identity, Name, Kind, Version string
	Size                          int64
}
type ReadRequest struct {
	Identity, Version string
	Offset, Limit     int64
}
type ReadResult struct {
	ContentRef, Version string
	NextOffset          int64
}
type SearchRequest struct {
	Root, Query, Cursor string
	Limit               int
}
type SearchResult struct {
	Matches    []SearchMatch
	NextCursor string
}
type SearchMatch struct {
	Identity string
	Line     int
	Preview  string
}

type AuthorizedFileWrite struct {
	Authorization   AuthorizedExecution `json:"-"`
	Path            string              `json:"path"`
	ContentRef      string              `json:"contentRef"`
	ExpectedVersion string              `json:"expectedVersion,omitempty"`
}
type AuthorizedFileEdit struct {
	Authorization   AuthorizedExecution `json:"-"`
	Path            string              `json:"path"`
	PatchRef        string              `json:"patchRef"`
	ExpectedVersion string              `json:"expectedVersion,omitempty"`
}
type FileEffect struct {
	Identity, Version, SideEffect string
	Confirmed                     bool
}

// ProcessOperations is the only process start/stop port.
type ProcessOperations interface {
	Execute(context.Context, AuthorizedProcess, ProgressSink) (ProcessObservation, error)
	Stop(context.Context, ExecutionRef) (StopObservation, error)
}

type AuthorizedProcess struct {
	Authorization    AuthorizedExecution `json:"-"`
	Argv             []string            `json:"argv"`
	Cwd              string              `json:"cwd,omitempty"`
	EnvironmentRef   string              `json:"environmentRef,omitempty"`
	StdinRef         string              `json:"stdinRef,omitempty"`
	Mounts           []ExecutionMount    `json:"mounts,omitempty"`
	TempRootRef      string              `json:"tempRootRef,omitempty"`
	OutputLimitBytes int                 `json:"outputLimitBytes,omitempty"`
}
type ProgressSink interface {
	WriteProgress(context.Context, ProcessProgress) error
}
type ProcessProgress struct {
	Stream, ContentRef string
	Sequence           uint64
}
type ProcessObservation struct {
	Execution                       ExecutionRef
	Started, Terminated             bool
	ExitCode                        int
	Content, ContentRef, SideEffect string
}
type ExecutionRef struct{ ID, BackendID, CallID string }
type StopObservation struct {
	Terminated bool
	SideEffect string
}

// ArtifactStore stores untrusted business output by reference; trusted ticket
// metadata stays outside the artifact body.
type ArtifactStore interface {
	Save(context.Context, ArtifactInput) (ArtifactRef, error)
	Open(context.Context, ArtifactRead) (io.ReadCloser, error)
}
type ArtifactInput struct {
	Authorization               AuthorizedExecution `json:"-"`
	ContentRef, MediaType, Name string
}
type ArtifactRead struct {
	Ref           ArtifactRef
	Offset, Limit int64
}
type ArtifactRef struct {
	ID, SessionID, Environment, Hash string
	Size                             int64
	Available                        bool
}
