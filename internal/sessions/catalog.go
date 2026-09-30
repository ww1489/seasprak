package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	sessstore "github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/jsonl"
)

// DefaultModelRef is the only model reference a catalog accepts in this revision.
const DefaultModelRef = "default"

// CatalogCreateRequest is an idempotent session creation request.
type CatalogCreateRequest struct {
	IdempotencyKey string
	Workspace      string
	ModelRef       string
}

// CatalogCreateResult returns the original initial snapshot, also for duplicates.
type CatalogCreateResult struct {
	Snapshot  Snapshot
	Duplicate bool
}

// CatalogListRequest pages sessions by ascending session ID after After.
type CatalogListRequest struct {
	After string
	Limit int
}

// CatalogEntry is rebuilt from journal headers; it is not a session fact.
type CatalogEntry struct {
	SessionID string
	Available bool
}

type CatalogListResult struct {
	Sessions []CatalogEntry
	Next     string
}

const catalogDefaultPage, catalogMaxPage = 50, 200

// Catalog owns creation idempotency and read-only browsing for one trusted
// workspace/state root. Created writers stay open until Close.
type Catalog struct {
	mu       sync.Mutex
	opts     Options
	registry sessstore.CreationRegistry
	writers  map[string]*AgentSession
	instance string
	closed   bool
	// files serializes attachment and metadata file publication.
	files sync.Mutex
}

// NewCatalog resolves the trusted roots and acquires the durable creation registry.
func NewCatalog(opts Options) (*Catalog, error) {
	if opts.Store != nil || opts.StateRoot == "memory" || opts.StateRoot == "" {
		return nil, product.NewError(product.CodeInvalidArgument, "catalog requires an explicit durable state root")
	}
	if opts.GenerationFingerprint == "" || opts.Principal == "" {
		return nil, product.NewError(product.CodeInvalidArgument, "catalog requires a principal and generation fingerprint")
	}
	ws, root, err := resolveRoots(opts, true)
	if err != nil {
		return nil, err
	}
	opts.Workspace, opts.StateRoot, opts.SessionID, opts.ReadOnly = ws, root, "", false
	registry, err := jsonl.OpenCreationRegistry(root)
	if err != nil {
		return nil, err
	}
	instance, err := agent.NewID()
	if err != nil {
		_ = registry.Close()
		return nil, err
	}
	return &Catalog{opts: opts, registry: registry, writers: map[string]*AgentSession{}, instance: instance}, nil
}

func (c *Catalog) usable() error {
	if c.closed {
		return product.NewError(product.CodeStateConflict, "session catalog is closed")
	}
	return nil
}

// authorize requires the requested workspace to be the trusted workspace.
func (c *Catalog) authorize(workspace string) error {
	real, err := sessstore.ResolveDir(workspace)
	if workspace == "" || err != nil || !sessstore.SamePath(real, c.opts.Workspace) {
		return product.NewError(product.CodePermissionDenied, "workspace is not served by this catalog")
	}
	return nil
}

func (c *Catalog) digest(req CatalogCreateRequest) string {
	raw, _ := json.Marshal([]string{c.opts.Principal, c.opts.Workspace, req.ModelRef})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func registryKey(principal, key string) string {
	raw, _ := json.Marshal([]string{principal, "create_session", key})
	return string(raw)
}

// Create serializes creation, reserving the session ID durably before any
// session file is written so retries after a crash or lost response reuse it.
func (c *Catalog) Create(ctx context.Context, req CatalogCreateRequest) (CatalogCreateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.usable(); err != nil {
		return CatalogCreateResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return CatalogCreateResult{}, err
	}
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 256 {
		return CatalogCreateResult{}, product.NewError(product.CodeInvalidArgument, "idempotency key is required")
	}
	if err := c.authorize(req.Workspace); err != nil {
		return CatalogCreateResult{}, err
	}
	key, digest := registryKey(c.opts.Principal, req.IdempotencyKey), c.digest(req)
	record, found, err := c.registry.Load(ctx, key)
	if err != nil {
		return CatalogCreateResult{}, err
	}
	if found && (record.Digest != digest || record.Generation != c.opts.GenerationFingerprint) {
		return CatalogCreateResult{}, product.NewError(product.CodeIdempotencyConflict, "key already belongs to a different session creation")
	}
	if found && len(record.Receipt) > 0 {
		snap, err := decodeReceipt(record.Receipt)
		return CatalogCreateResult{Snapshot: snap, Duplicate: true}, err
	}
	if req.ModelRef != DefaultModelRef {
		return CatalogCreateResult{}, product.NewError(product.CodeInvalidArgument, "model reference is not available")
	}
	// Reject before reserving or writing anything: Start refuses writers in the
	// default profile, and a half-created journal must not be left behind.
	if c.opts.Profile != ProfileMemory {
		return CatalogCreateResult{}, product.NewError(product.CodeResourceUnavailable, "default file and process backends are not available")
	}
	if !found {
		sid, err := agent.NewID()
		if err != nil {
			return CatalogCreateResult{}, err
		}
		record = sessstore.CreationRecord{SessionID: sid, Digest: digest, Generation: c.opts.GenerationFingerprint}
		if err := c.registry.Save(ctx, key, record); err != nil {
			return CatalogCreateResult{}, err
		}
	}
	session, err := c.materialize(ctx, record.SessionID)
	if err != nil {
		return CatalogCreateResult{}, err
	}
	snap, err := session.Snapshot(ctx)
	if err != nil {
		return CatalogCreateResult{}, err
	}
	receipt, err := json.Marshal(snap)
	if err != nil {
		return CatalogCreateResult{}, product.NewError(product.CodeInternal, "initial snapshot cannot be encoded")
	}
	record.Receipt = receipt
	if err := c.registry.Save(ctx, key, record); err != nil {
		return CatalogCreateResult{}, err
	}
	snap, err = decodeReceipt(receipt)
	return CatalogCreateResult{Snapshot: snap, Duplicate: found}, err
}

// materialize creates the reserved session, or reopens it when an earlier
// attempt already wrote its journal. It never allocates a different ID.
func (c *Catalog) materialize(ctx context.Context, sid string) (*AgentSession, error) {
	if s := c.writers[sid]; s != nil {
		return s, nil
	}
	opts := c.opts
	opts.SessionID = sid
	var (
		session *AgentSession
		err     error
	)
	if _, statErr := os.Lstat(journalPath(opts.StateRoot, sid)); statErr == nil {
		session, err = OpenAgentSession(ctx, opts)
	} else if os.IsNotExist(statErr) {
		session, err = CreateAgentSession(ctx, opts)
	} else {
		err = product.NewError(product.CodeStorageUnavailable, "reserved session cannot be inspected")
	}
	if err != nil {
		return nil, err
	}
	c.writers[sid] = session
	return session, nil
}

// Writer returns the single writable session for sid, opening it once when it
// is served by this catalog. Only mutating routes call Writer; browsing uses
// Snapshot, which never opens a writer.
func (c *Catalog) Writer(ctx context.Context, sid string) (*AgentSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.usable(); err != nil {
		return nil, err
	}
	if err := sessstore.ValidateResourceID(sid); err != nil || (c.writers[sid] == nil && !c.served(sid)) {
		return nil, product.NewError(product.CodeNotFound, "session does not exist")
	}
	if c.opts.Profile != ProfileMemory {
		return nil, product.NewError(product.CodeResourceUnavailable, "default file and process backends are not available")
	}
	if s := c.writers[sid]; s != nil {
		return s, nil
	}
	opts := c.opts
	opts.SessionID = sid
	s, err := OpenAgentSession(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.writers[sid] = s
	return s, nil
}

// Browse returns the open writer for sid, or a temporary read-only session.
// The returned func releases the read-only session; it is a no-op for writers.
func (c *Catalog) Browse(ctx context.Context, sid string) (*AgentSession, func(), error) {
	c.mu.Lock()
	if err := c.usable(); err != nil {
		c.mu.Unlock()
		return nil, nil, err
	}
	if err := sessstore.ValidateResourceID(sid); err != nil {
		c.mu.Unlock()
		return nil, nil, product.NewError(product.CodeNotFound, "session does not exist")
	}
	writer, opts := c.writers[sid], c.opts
	c.mu.Unlock()
	if writer != nil {
		return writer, func() {}, nil
	}
	if !c.served(sid) {
		return nil, nil, product.NewError(product.CodeNotFound, "session does not exist")
	}
	opts.SessionID, opts.ReadOnly = sid, true
	s, err := OpenAgentSession(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	return s, func() { _ = s.Close(context.Background()) }, nil
}

// Subscribe replays durable events after cursor. With an open writer it then
// continues live; otherwise it replays through a temporary read-only open and
// the event channel closes after history (Live reports false). It never opens
// a writer, so observing cannot pause, hold or resume work.
func (c *Catalog) Subscribe(ctx context.Context, sid string, after uint64, limits config.Limits) (*ReplaySubscription, bool, func(), error) {
	c.mu.Lock()
	if err := c.usable(); err != nil {
		c.mu.Unlock()
		return nil, false, nil, err
	}
	if err := sessstore.ValidateResourceID(sid); err != nil {
		c.mu.Unlock()
		return nil, false, nil, product.NewError(product.CodeNotFound, "session does not exist")
	}
	writer, opts := c.writers[sid], c.opts
	c.mu.Unlock()
	if writer != nil {
		r, err := writer.SubscribeFrom(ctx, after, limits)
		if err != nil {
			return nil, false, nil, err
		}
		return r, true, r.Close, nil
	}
	if !c.served(sid) {
		return nil, false, nil, product.NewError(product.CodeNotFound, "session does not exist")
	}
	opts.SessionID, opts.ReadOnly = sid, true
	session, err := OpenAgentSession(ctx, opts)
	if err != nil {
		return nil, false, nil, err
	}
	// A read-only session never commits, so after history the replay ends.
	r, err := session.subscribeFrom(ctx, after, limits, true)
	if err != nil {
		_ = session.Close(context.WithoutCancel(ctx))
		return nil, false, nil, err
	}
	return r, false, func() { r.Close(); _ = session.Close(context.Background()) }, nil
}

// InstanceID identifies this catalog process instance. Approval receipts are
// valid only within one instance; a new process yields a new ID.
func (c *Catalog) InstanceID() string { return c.instance }

func decodeReceipt(raw json.RawMessage) (Snapshot, error) {
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, product.NewError(product.CodeStorageUnavailable, "creation receipt is corrupt")
	}
	return snap, nil
}

// Snapshot browses one session without writing or resuming it. Sessions bound
// to another workspace are reported as not found.
func (c *Catalog) Snapshot(ctx context.Context, sid string) (Snapshot, error) {
	c.mu.Lock()
	if err := c.usable(); err != nil {
		c.mu.Unlock()
		return Snapshot{}, err
	}
	if err := sessstore.ValidateResourceID(sid); err != nil {
		c.mu.Unlock()
		return Snapshot{}, product.NewError(product.CodeNotFound, "session does not exist")
	}
	writer := c.writers[sid]
	opts := c.opts
	c.mu.Unlock()
	if writer != nil {
		return writer.Snapshot(ctx)
	}
	if !c.served(sid) {
		return Snapshot{}, product.NewError(product.CodeNotFound, "session does not exist")
	}
	opts.SessionID, opts.ReadOnly = sid, true
	session, err := OpenAgentSession(ctx, opts)
	if err != nil {
		return Snapshot{}, err
	}
	snap, err := session.Snapshot(ctx)
	closeErr := session.Close(context.WithoutCancel(ctx))
	if err == nil {
		err = closeErr
	}
	return snap, err
}

// served reports whether the journal header binds sid to the trusted workspace.
func (c *Catalog) served(sid string) bool {
	header, err := readHeader(c.opts.StateRoot, sid, c.opts.Limits.WithDefaults().MaxCommitLineBytes)
	if err != nil {
		return false
	}
	binding, err := decodeBinding(header.Workspace)
	if err != nil {
		return false
	}
	real, err := sessstore.ResolveDir(binding.HostRealRoot)
	return err == nil && sessstore.SamePath(real, c.opts.Workspace)
}

// List enumerates journal headers read-only. Unreadable sessions are listed as
// unavailable; sessions bound to other workspaces are omitted.
func (c *Catalog) List(ctx context.Context, req CatalogListRequest) (CatalogListResult, error) {
	c.mu.Lock()
	err := c.usable()
	root := c.opts.StateRoot
	c.mu.Unlock()
	if err != nil {
		return CatalogListResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return CatalogListResult{}, err
	}
	limit := req.Limit
	if limit <= 0 {
		limit = catalogDefaultPage
	}
	if limit > catalogMaxPage {
		return CatalogListResult{}, product.NewError(product.CodeInvalidArgument, "page size exceeds limit")
	}
	entries, err := os.ReadDir(filepath.Join(root, "sessions"))
	if os.IsNotExist(err) {
		return CatalogListResult{}, nil
	}
	if err != nil {
		return CatalogListResult{}, product.NewError(product.CodeStorageUnavailable, "session directory is unavailable")
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && e.Type()&os.ModeSymlink == 0 && sessstore.ValidateResourceID(e.Name()) == nil && e.Name() > req.After {
			ids = append(ids, e.Name())
		}
	}
	slices.Sort(ids)
	out := CatalogListResult{Sessions: []CatalogEntry{}}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return CatalogListResult{}, err
		}
		if len(out.Sessions) == limit {
			out.Next = out.Sessions[limit-1].SessionID
			break
		}
		header, err := readHeader(root, id, c.opts.Limits.WithDefaults().MaxCommitLineBytes)
		if err != nil {
			out.Sessions = append(out.Sessions, CatalogEntry{SessionID: id})
			continue
		}
		if binding, err := decodeBinding(header.Workspace); err != nil {
			out.Sessions = append(out.Sessions, CatalogEntry{SessionID: id})
		} else if real, err := sessstore.ResolveDir(binding.HostRealRoot); err == nil && sessstore.SamePath(real, c.opts.Workspace) {
			out.Sessions = append(out.Sessions, CatalogEntry{SessionID: id, Available: true})
		}
	}
	return out, nil
}

// Close closes created writers and releases the registry writer lock.
func (c *Catalog) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	var first error
	for id, s := range c.writers {
		if err := s.Close(ctx); err != nil && first == nil {
			first = err
		}
		delete(c.writers, id)
	}
	if err := c.registry.Close(); err != nil && first == nil {
		first = err
	}
	return first
}
