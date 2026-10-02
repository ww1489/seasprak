package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

const DefaultModelRef = "default"

type CatalogCreateRequest struct{ IdempotencyKey, Workspace, ModelRef string }
type CatalogCreateResult struct {
	Snapshot  codeagent.Snapshot
	Duplicate bool
}
type CatalogListRequest struct {
	After string
	Limit int
}
type CatalogEntry struct {
	SessionID string
	Available bool
}
type CatalogListResult struct {
	Sessions []CatalogEntry
	Next     string
}

const catalogDefaultPage, catalogMaxPage = 50, 200

// Catalog owns application creation, writer caching and browsing for one
// trusted workspace. Execution remains behind controlled Code session methods.
type Catalog struct {
	mu               sync.Mutex
	opts             codeagent.Options
	registry         store.CreationRegistry
	ownsRegistry     bool
	writers          map[string]*codeagent.AgentSession
	instance         string
	closed           bool
	files            sync.Mutex
	publish          func(*os.Root, string, string, []byte) error
	activeFileWrites int
	fileWritesDone   chan struct{}
}

func NewCatalog(opts codeagent.Options) (*Catalog, error) {
	return newCatalog(opts, nil)
}

// A supplied registry belongs to the combined resource owner, not this catalog.
func newCatalog(opts codeagent.Options, registry store.CreationRegistry) (*Catalog, error) {
	if opts.Store != nil || opts.StateRoot == "memory" || opts.StateRoot == "" {
		return nil, product.NewError(product.CodeInvalidArgument, "catalog requires an explicit durable state root")
	}
	if opts.GenerationFingerprint == "" || opts.Principal == "" {
		return nil, product.NewError(product.CodeInvalidArgument, "catalog requires a principal and generation fingerprint")
	}
	ws, err := store.ResolveDir(opts.Workspace)
	if err != nil {
		return nil, err
	}
	root, err := store.ResolveDir(opts.StateRoot)
	if err != nil {
		return nil, err
	}
	if store.PathsOverlap(ws, root) {
		return nil, product.NewError(product.CodeInvalidArgument, "state root overlaps the workspace")
	}
	opts.Workspace, opts.StateRoot, opts.SessionID, opts.ReadOnly = ws, root, "", false
	owned := registry == nil
	if owned {
		registry, err = store.OpenCreationRegistry(root)
		if err != nil {
			return nil, err
		}
	}
	instance, err := agent.NewID()
	if err != nil {
		if owned {
			_ = registry.Close()
		}
		return nil, err
	}
	return &Catalog{opts: opts, registry: registry, ownsRegistry: owned, writers: map[string]*codeagent.AgentSession{}, instance: instance, publish: publishFile}, nil
}
func (c *Catalog) usable() error {
	if c.closed {
		return product.NewError(product.CodeStateConflict, "session catalog is closed")
	}
	return nil
}

// beginFileWrite is called only after files is locked and validation succeeds.
// Its admission shares mu with Close, so ownership lasts through all real IO.
func (c *Catalog) beginFileWrite(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.usable(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.activeFileWrites == 0 {
		c.fileWritesDone = make(chan struct{})
	}
	c.activeFileWrites++
	return nil
}

func (c *Catalog) finishFileWrite() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeFileWrites--
	if c.activeFileWrites == 0 {
		close(c.fileWritesDone)
	}
}

func (c *Catalog) authorize(workspace string) error {
	real, err := store.ResolveDir(workspace)
	if workspace == "" || err != nil || !store.SamePath(real, c.opts.Workspace) {
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

// Create reserves the ID before materialization, and persists the original
// snapshot receipt. Retries never allocate another ID or regenerate a receipt.
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
	if found && (record.ResourceType != "" && record.ResourceType != store.ResourceCode || record.RunID != "" || record.Digest != digest || record.Generation != c.opts.GenerationFingerprint) {
		return CatalogCreateResult{}, product.NewError(product.CodeIdempotencyConflict, "key already belongs to a different session creation")
	}
	if found && len(record.Receipt) > 0 {
		snap, err := decodeReceipt(record.Receipt)
		return CatalogCreateResult{Snapshot: snap, Duplicate: true}, err
	}
	if req.ModelRef != DefaultModelRef {
		return CatalogCreateResult{}, product.NewError(product.CodeInvalidArgument, "model reference is not available")
	}
	if c.opts.Profile != codeagent.ProfileMemory {
		return CatalogCreateResult{}, product.NewError(product.CodeResourceUnavailable, "default file and process backends are not available")
	}
	if !found {
		sid, err := agent.NewID()
		if err != nil {
			return CatalogCreateResult{}, err
		}
		record = store.CreationRecord{SessionID: sid, Digest: digest, Generation: c.opts.GenerationFingerprint}
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
func (c *Catalog) materialize(ctx context.Context, sid string) (*codeagent.AgentSession, error) {
	if s := c.writers[sid]; s != nil {
		return s, nil
	}
	opts := c.opts
	opts.SessionID = sid
	var session *codeagent.AgentSession
	_, err := codeagent.InspectSessionHeader(ctx, opts.StateRoot, sid, opts.Limits)
	if err == nil {
		session, err = codeagent.OpenAgentSession(ctx, opts)
	} else if pe, ok := product.AsError(err); ok && pe.Code == product.CodeNotFound {
		session, err = codeagent.CreateAgentSession(ctx, opts)
	}
	if err != nil {
		return nil, err
	}
	c.writers[sid] = session
	return session, nil
}
func (c *Catalog) Writer(ctx context.Context, sid string) (*codeagent.AgentSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.usable(); err != nil {
		return nil, err
	}
	if err := store.ValidateResourceID(sid); err != nil || (c.writers[sid] == nil && !c.served(ctx, sid)) {
		return nil, product.NewError(product.CodeNotFound, "session does not exist")
	}
	if c.opts.Profile != codeagent.ProfileMemory {
		return nil, product.NewError(product.CodeResourceUnavailable, "default file and process backends are not available")
	}
	if s := c.writers[sid]; s != nil {
		return s, nil
	}
	opts := c.opts
	opts.SessionID = sid
	s, err := codeagent.OpenAgentSession(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.writers[sid] = s
	return s, nil
}
func (c *Catalog) Browse(ctx context.Context, sid string) (*codeagent.AgentSession, func(), error) {
	c.mu.Lock()
	if err := c.usable(); err != nil {
		c.mu.Unlock()
		return nil, nil, err
	}
	if err := store.ValidateResourceID(sid); err != nil {
		c.mu.Unlock()
		return nil, nil, product.NewError(product.CodeNotFound, "session does not exist")
	}
	writer, opts := c.writers[sid], c.opts
	c.mu.Unlock()
	if writer != nil {
		return writer, func() {}, nil
	}
	if !c.served(ctx, sid) {
		return nil, nil, product.NewError(product.CodeNotFound, "session does not exist")
	}
	opts.SessionID, opts.ReadOnly = sid, true
	s, err := codeagent.OpenAgentSession(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	return s, func() { _ = s.Close(context.Background()) }, nil
}
func (c *Catalog) Subscribe(ctx context.Context, sid string, after uint64, limits config.Limits) (*codeagent.ReplaySubscription, bool, func(), error) {
	c.mu.Lock()
	if err := c.usable(); err != nil {
		c.mu.Unlock()
		return nil, false, nil, err
	}
	if err := store.ValidateResourceID(sid); err != nil {
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
	if !c.served(ctx, sid) {
		return nil, false, nil, product.NewError(product.CodeNotFound, "session does not exist")
	}
	opts.SessionID, opts.ReadOnly = sid, true
	s, err := codeagent.OpenAgentSession(ctx, opts)
	if err != nil {
		return nil, false, nil, err
	}
	r, err := codeagent.SubscribeHistoryFrom(ctx, s, after, limits)
	if err != nil {
		_ = s.Close(context.WithoutCancel(ctx))
		return nil, false, nil, err
	}
	return r, false, func() { r.Close(); _ = s.Close(context.Background()) }, nil
}
func (c *Catalog) InstanceID() string { return c.instance }
func decodeReceipt(raw json.RawMessage) (codeagent.Snapshot, error) {
	var snap codeagent.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return codeagent.Snapshot{}, product.NewError(product.CodeStorageUnavailable, "creation receipt is corrupt")
	}
	return snap, nil
}
func (c *Catalog) Snapshot(ctx context.Context, sid string) (codeagent.Snapshot, error) {
	c.mu.Lock()
	if err := c.usable(); err != nil {
		c.mu.Unlock()
		return codeagent.Snapshot{}, err
	}
	if err := store.ValidateResourceID(sid); err != nil {
		c.mu.Unlock()
		return codeagent.Snapshot{}, product.NewError(product.CodeNotFound, "session does not exist")
	}
	writer, opts := c.writers[sid], c.opts
	c.mu.Unlock()
	if writer != nil {
		return writer.Snapshot(ctx)
	}
	if !c.served(ctx, sid) {
		return codeagent.Snapshot{}, product.NewError(product.CodeNotFound, "session does not exist")
	}
	opts.SessionID, opts.ReadOnly = sid, true
	s, err := codeagent.OpenAgentSession(ctx, opts)
	if err != nil {
		return codeagent.Snapshot{}, err
	}
	snap, err := s.Snapshot(ctx)
	closeErr := s.Close(context.WithoutCancel(ctx))
	if err == nil {
		err = closeErr
	}
	return snap, err
}
func (c *Catalog) served(ctx context.Context, sid string) bool {
	desc, err := codeagent.InspectSessionHeader(ctx, c.opts.StateRoot, sid, c.opts.Limits)
	if err != nil {
		return false
	}
	real, err := store.ResolveDir(desc.Workspace)
	return err == nil && store.SamePath(real, c.opts.Workspace)
}
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
	sessions := filepath.Join(root, "sessions")
	info, err := os.Lstat(sessions)
	if os.IsNotExist(err) {
		return CatalogListResult{}, nil
	}
	if err != nil || !info.IsDir() {
		return CatalogListResult{}, product.NewError(product.CodeStorageUnavailable, "session directory is unavailable")
	}
	if linked, err := store.IsReparse(sessions); err != nil || linked {
		return CatalogListResult{}, product.NewError(product.CodeStorageUnavailable, "session directory has invalid type")
	}
	directory, err := os.OpenRoot(sessions)
	if err != nil {
		return CatalogListResult{}, product.NewError(product.CodeStorageUnavailable, "session directory is unavailable")
	}
	defer directory.Close()
	f, err := directory.Open(".")
	if err != nil {
		return CatalogListResult{}, product.NewError(product.CodeStorageUnavailable, "session directory is unavailable")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return CatalogListResult{}, product.NewError(product.CodeStorageUnavailable, "session directory changed while opening")
	}
	entries, err := f.ReadDir(-1)
	if err != nil {
		return CatalogListResult{}, product.NewError(product.CodeStorageUnavailable, "session directory is unavailable")
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && e.Type()&os.ModeSymlink == 0 && store.ValidateResourceID(e.Name()) == nil && e.Name() > req.After {
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
		desc, err := codeagent.InspectSessionHeader(ctx, root, id, c.opts.Limits)
		if err != nil {
			out.Sessions = append(out.Sessions, CatalogEntry{SessionID: id})
			continue
		}
		if real, err := store.ResolveDir(desc.Workspace); err == nil && store.SamePath(real, c.opts.Workspace) {
			out.Sessions = append(out.Sessions, CatalogEntry{SessionID: id, Available: true})
		}
	}
	return out, nil
}

// Close marks the catalog closed to new business immediately. A timeout keeps
// every writer or admitted file write that has not completed, and the registry
// lock. A later Close can continue waiting for the same real completion facts.
func (c *Catalog) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	writers := make(map[string]*codeagent.AgentSession, len(c.writers))
	for id, s := range c.writers {
		writers[id] = s
	}
	var fileWritesDone <-chan struct{}
	if c.activeFileWrites != 0 {
		fileWritesDone = c.fileWritesDone
	}
	c.mu.Unlock()
	var closeErr error
	for id, s := range writers {
		if err := s.Close(ctx); err != nil {
			closeErr = errors.Join(closeErr, err)
			// A context error proves only that this caller stopped waiting.
			// Other Close errors are returned after the session has exited.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
		}
		c.mu.Lock()
		delete(c.writers, id)
		c.mu.Unlock()
	}
	if fileWritesDone != nil {
		select {
		case <-fileWritesDone:
		case <-ctx.Done():
			return errors.Join(closeErr, ctx.Err())
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ownsRegistry && len(c.writers) == 0 && c.activeFileWrites == 0 {
		closeErr = errors.Join(closeErr, c.registry.Close())
	}
	return closeErr
}
