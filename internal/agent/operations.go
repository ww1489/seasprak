package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

// ListRequest asks for raw directory entries. The builtin owns sorting and
// output limits; a backend that cannot return all entries must set NextCursor
// so the output explicitly reports incompleteness, not silently drop entries.
// Cursor is retained for host compatibility; builtins do not paginate.
type ListRequest struct {
	Root, Cursor string
	Limit        int
}
type ListResult struct {
	Entries    []FileEntry
	NextCursor string
}
type FileEntry struct {
	Identity, Name, Kind, Version string
	Size                          int64
}

// ReadRequest binds a read_file range to a complete, immutable file snapshot.
// For Mode "lines" or "bytes", Offset and Limit describe the requested range
// (zero-based lines or UTF-8 bytes), but MUST NOT pre-slice the returned snapshot.
// They remain bound to FrozenExecution for the artifact backend to verify.
// Version, when nonempty, must match the snapshot or Read returns state_conflict.
//
// The built-in tool always supplies an explicit Mode. Injected backends must
// migrate to this snapshot contract; empty Mode is not a compatibility fallback
// for the built-in tool, even if a backend retains its own legacy empty mode.
type ReadRequest struct {
	Identity, Version string
	Mode              string
	Offset, Limit     int64
}

// ReadResult identifies the complete snapshot, not an inline body or a range.
// ContentRef must be readable through the injected ArtifactStore, and Version
// must be nonempty and identify the immutable bytes exposed by that reference.
// The tool calls Open with Offset=0 and Limit=0 under the frozen execution
// authority, then streams and projects the requested range itself. Open must
// verify that authority and its bound request before exposing snapshot bytes.
// A snapshot reference need not copy or buffer the entire file in memory.
// NextOffset is legacy backend metadata; the built-in tool does not use it for
// explicit modes and derives version-bound continuation from projected bytes.
type ReadResult struct {
	ContentRef, Version string
	NextOffset          int64
}

// SearchRequest binds matching and projection options. Return raw, unsliced
// matches: builtin tools apply OutputMode, Offset and Limit exactly once.
// A backend returning a partial set must set NextCursor; counts then describe
// only that set and the builtin reports incomplete results. Paths are logical
// slash-separated paths. grep uses Go regular expressions, glob uses doublestar.
type SearchRequest struct {
	Root, Query, Cursor string
	// Kind distinguishes glob path matching from grep content matching.
	Kind, Glob, OutputMode string
	CaseInsensitive        bool
	Offset, Limit          int
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
	PatchRef        string              `json:"patchRef,omitempty"`
	OldString       string              `json:"old_string"`
	NewString       string              `json:"new_string"`
	ReplaceAll      bool                `json:"replace_all,omitempty"`
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
	Shell            string              `json:"shell,omitempty"`
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

// ToolOutputChunk contains only text the trusted tool has prepared for SDK subscribers.
type ToolOutputChunk struct {
	Stream, Text string
}

type ToolOutputSink interface {
	WriteOutput(context.Context, ToolOutputChunk) error
}

// ToolOutputDelta is the allowlisted payload of a temporary tool.output.delta event.
type ToolOutputDelta struct {
	ToolCallID string `json:"toolCallId"`
	Stream     string `json:"stream"`
	Text       string `json:"text"`
}

// ToolOutputFact is internal publication metadata; CallID and ToolCallID
// both identify the accepted product call, never a backend-supplied identity.
type ToolOutputFact struct {
	ToolOutputDelta
	CallID   string `json:"callId"`
	StreamID string `json:"streamId"`
	ChunkSeq uint64 `json:"chunkSeq"`
}

type ProcessProgress struct {
	Stream, ContentRef string
	Text               string
	Sequence           uint64
}

// Content is complete inline output for optional log post-processing. A
// ContentRef alone is opaque and is never opened with a consumed process ticket.
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

type TodoOperations interface {
	Update(context.Context, AuthorizedTodo) (TodoEffect, error)
}

type AuthorizedTodo struct {
	Authorization AuthorizedExecution `json:"-"`
	InvocationID  string              `json:"invocationId"`
	Content       json.RawMessage     `json:"content"`
}
type TodoEffect struct {
	Version   string
	Content   string
	Confirmed bool
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
	Authorization AuthorizedExecution `json:"-"`
	Ref           ArtifactRef
	Offset, Limit int64
}

// OutputArtifactStore is an optional trusted post-processing port implemented
// by an injected ArtifactStore. It cannot start execution or grant permissions.
// Content must already be redacted; bindings are supplied by the executor, not
// tool arguments. Implementations isolate bindings and verify refs on reads.
type OutputArtifactStore interface {
	SaveOutput(context.Context, OutputArtifactInput) (ArtifactRef, error)
	OpenOutput(context.Context, OutputArtifactRead) (io.ReadCloser, error)
}

type OutputArtifactBinding struct {
	SessionID, Environment, CallID string
}
type OutputArtifactInput struct {
	Binding                  OutputArtifactBinding
	Content, MediaType, Name string
}
type OutputArtifactRead struct {
	Binding OutputArtifactBinding
	Ref     ArtifactRef
}

// OutputRedactor is trusted host configuration, never a tool argument. Errors
// and panics must not publish the original text or change execution facts.
type OutputRedactor func(context.Context, string) (string, error)

type ArtifactRef struct {
	ID, SessionID, Environment, Hash string
	Size                             int64
	Available                        bool
}
