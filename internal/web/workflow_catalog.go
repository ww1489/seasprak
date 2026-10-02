package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/workflowagent"
)

var errWorkflowForeignWorkspace = errors.New("workflow workspace is not served")

type workflowCreateRequest struct {
	IdempotencyKey, Workspace, Workflow, Version string
	Input                                        json.RawMessage
}
type workflowCatalog struct {
	mu                                  sync.Mutex
	workspace, root, principal, profile string
	options                             map[string]workflowagent.WorkflowOptions
	compiled                            map[string]*workflowagent.CompiledWorkflow
	registry                            storage.CreationRegistry // Borrowed from the combined resource owner.
	writers                             map[string]*workflowagent.WorkflowAgent
	closed                              bool
}
type workflowEntryDTO struct {
	RunID             string `json:"runId"`
	DefinitionName    string `json:"definitionName"`
	DefinitionVersion string `json:"definitionVersion"`
	State             string `json:"state"`
	Available         bool   `json:"available"`
}
type workflowListDTO struct {
	Runs []workflowEntryDTO `json:"runs"`
	Next string             `json:"next,omitempty"`
}

func newWorkflowCatalog(code codeagent.Options, options map[string]workflowagent.WorkflowOptions, registry storage.CreationRegistry) (*workflowCatalog, error) {
	c := &workflowCatalog{workspace: code.Workspace, root: code.StateRoot, principal: code.Principal, profile: code.Profile, options: map[string]workflowagent.WorkflowOptions{}, compiled: map[string]*workflowagent.CompiledWorkflow{}, registry: registry, writers: map[string]*workflowagent.WorkflowAgent{}}
	for key, opts := range options {
		if key != opts.Definition.Name+"@"+opts.Definition.Version || opts.Principal != c.principal || opts.GenerationFingerprint == "" || opts.Store != nil || !storage.SamePath(opts.Workspace, c.workspace) || !storage.SamePath(opts.StateRoot, c.root) {
			return nil, invalid("workflow catalog binding is invalid")
		}
		// Detach all declarative values from caller-owned startup maps and slices.
		raw, _ := json.Marshal(opts.Definition)
		opts.Definition = workflowagent.WorkflowDefinition{}
		_ = json.Unmarshal(raw, &opts.Definition)
		raw, _ = json.Marshal(opts.Subflows)
		opts.Subflows = nil
		_ = json.Unmarshal(raw, &opts.Subflows)
		opts.RunID, opts.ReadOnly = "", false
		opts.Models = maps.Clone(opts.Models)
		if opts.Policy != nil {
			policy := *opts.Policy
			opts.Policy = &policy
		}
		opts.Tools = slices.Clone(opts.Tools)
		for i := range opts.Tools {
			opts.Tools[i] = opts.Tools[i].Clone()
		}
		bindings := workflowagent.WorkflowBindings{Models: map[string]bool{}, Tools: map[string]json.RawMessage{}, Subflows: map[string]*workflowagent.CompiledWorkflow{}}
		for name, m := range opts.Models {
			bindings.Models[name] = m != nil
		}
		for _, def := range opts.Tools {
			bindings.Tools[def.Name] = append(json.RawMessage(nil), def.Schema...)
		}
		visiting := map[string]bool{}
		var subflow func(string) (*workflowagent.CompiledWorkflow, error)
		subflow = func(id string) (*workflowagent.CompiledWorkflow, error) {
			if compiled := bindings.Subflows[id]; compiled != nil {
				return compiled, nil
			}
			def, found := opts.Subflows[id]
			if !found || id != def.Name+"@"+def.Version || visiting[id] {
				return nil, invalid("workflow subflow binding is unavailable")
			}
			visiting[id] = true
			defer delete(visiting, id)
			for _, node := range def.Nodes {
				if node.Type == workflowagent.WorkflowNodeSubflow {
					if _, err := subflow(node.Subflow); err != nil {
						return nil, err
					}
				}
			}
			compiled, err := workflowagent.CompileWorkflow(def, bindings)
			if err == nil {
				bindings.Subflows[id] = compiled
			}
			return compiled, err
		}
		for id := range opts.Subflows {
			if _, err := subflow(id); err != nil {
				return nil, err
			}
		}
		compiled, err := workflowagent.CompileWorkflow(opts.Definition, bindings)
		if err != nil {
			return nil, err
		}
		c.options[key], c.compiled[key] = opts, compiled
	}
	return c, nil
}
func (c *workflowCatalog) usable() error {
	if c.closed {
		return product.NewError(product.CodeStateConflict, "workflow catalog is closed")
	}
	return nil
}
func workflowRegistryKey(principal, key string) string {
	raw, _ := json.Marshal([]string{principal, "create_workflow_run", key})
	return string(raw)
}
func canonicalWorkflowInput(raw json.RawMessage) (json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v map[string]any
	if d.Decode(&v) != nil || v == nil || d.Decode(new(any)) != io.EOF {
		return nil, invalid("workflow input must be one JSON object")
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, invalid("workflow input is invalid")
	}
	return out, nil
}
func (c *workflowCatalog) digest(req workflowCreateRequest) string {
	raw, _ := json.Marshal(struct {
		Principal, Workspace, Workflow, Version string
		Input                                   json.RawMessage
	}{c.principal, c.workspace, req.Workflow, req.Version, req.Input})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Create reserves the independent root before touching the run. The internal
// input key is fixed by that reservation, including after lost HTTP responses.
func (c *workflowCatalog) Create(ctx context.Context, req workflowCreateRequest) (workflowSnapshotDTO, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.usable(); err != nil {
		return workflowSnapshotDTO{}, err
	}
	if err := ctx.Err(); err != nil {
		return workflowSnapshotDTO{}, err
	}
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 256 {
		return workflowSnapshotDTO{}, invalid("idempotency key is required")
	}
	if !filepath.IsAbs(req.Workspace) {
		return workflowSnapshotDTO{}, product.NewError(product.CodePermissionDenied, "workspace is not served by this catalog")
	}
	workspace, err := storage.ResolveDir(req.Workspace)
	if err != nil || !storage.SamePath(workspace, c.workspace) {
		return workflowSnapshotDTO{}, product.NewError(product.CodePermissionDenied, "workspace is not served by this catalog")
	}
	opts, found := c.options[req.Workflow+"@"+req.Version]
	if !found {
		return workflowSnapshotDTO{}, invalid("workflow definition is not available")
	}
	if c.profile != codeagent.ProfileMemory {
		return workflowSnapshotDTO{}, product.NewError(product.CodeResourceUnavailable, "workflow execution profile is unavailable")
	}
	req.Input, err = canonicalWorkflowInput(req.Input)
	if err != nil {
		return workflowSnapshotDTO{}, err
	}
	if err := workflowagent.ValidateWorkflowInput(c.compiled[req.Workflow+"@"+req.Version], req.Input); err != nil {
		return workflowSnapshotDTO{}, invalid("workflow input does not match its schema")
	}
	key, digest := workflowRegistryKey(c.principal, req.IdempotencyKey), c.digest(req)
	record, found, err := c.registry.Load(ctx, key)
	if err != nil {
		return workflowSnapshotDTO{}, err
	}
	if found && (record.ResourceType != storage.ResourceWorkflow || record.SessionID != "" || record.Digest != digest || record.Generation != opts.GenerationFingerprint) {
		return workflowSnapshotDTO{}, product.NewError(product.CodeIdempotencyConflict, "key already belongs to a different workflow creation")
	}
	if found && len(record.Receipt) != 0 {
		return decodeWorkflowReceipt(record.Receipt, record.RunID)
	}
	if !found {
		rid, err := agent.NewID()
		if err != nil {
			return workflowSnapshotDTO{}, err
		}
		record = storage.CreationRecord{ResourceType: storage.ResourceWorkflow, RunID: rid, Digest: digest, Generation: opts.GenerationFingerprint}
		if err := c.registry.Save(ctx, key, record); err != nil {
			return workflowSnapshotDTO{}, err
		}
	}
	// Admission is durable. Disconnects after reservation cannot cancel the run
	// or turn a successful input into a second creation on retry.
	acceptedCtx := context.WithoutCancel(ctx)
	run, err := c.materialize(acceptedCtx, record.RunID, opts)
	if err != nil {
		return workflowSnapshotDTO{}, err
	}
	receipt, err := run.SubmitInput(acceptedCtx, workflowagent.WorkflowInputCommand{Input: req.Input, IdempotencyKey: "web-create:" + record.RunID, Principal: c.principal})
	if err != nil {
		return workflowSnapshotDTO{}, err
	}
	initial, err := acceptedWorkflowSnapshot(acceptedCtx, run, receipt)
	if err != nil {
		return workflowSnapshotDTO{}, err
	}
	raw, err := json.Marshal(initial)
	if err != nil {
		return workflowSnapshotDTO{}, product.NewError(product.CodeInternal, "workflow receipt cannot be encoded")
	}
	record.Receipt = raw
	if err := c.registry.Save(acceptedCtx, key, record); err != nil {
		return workflowSnapshotDTO{}, err
	}
	return decodeWorkflowReceipt(raw, record.RunID)
}

// Select the original input acceptance and its adjacent running event, not a
// raced final Snapshot. Both are supplied by the upper layer's atomic replay.
func acceptedWorkflowSnapshot(ctx context.Context, run *workflowagent.WorkflowAgent, receipt workflowagent.WorkflowInputReceipt) (workflowSnapshotDTO, error) {
	snapshot, err := run.Snapshot(ctx)
	if err != nil {
		return workflowSnapshotDTO{}, err
	}
	sub, err := run.SubscribeFrom(ctx, workflowagent.WorkflowSubscribeOptions{})
	if err != nil {
		return workflowSnapshotDTO{}, err
	}
	defer sub.Close()
	accepted := false
	for ev := range sub.Events {
		if ev.DurableSeq == nil {
			continue
		}
		if ev.Type == "workflow.input.accepted" {
			var input workflowagent.WorkflowInputReceipt
			if json.Unmarshal(ev.Payload, &input) == nil && input == receipt {
				accepted = true
			}
			continue
		}
		if accepted {
			var state struct {
				State string `json:"state"`
			}
			if ev.Type != "workflow.state_changed" || json.Unmarshal(ev.Payload, &state) != nil || state.State != "running" {
				break
			}
			snapshot.State, snapshot.Revision, snapshot.DurableSeq = state.State, receipt.AcceptedCommit, *ev.DurableSeq
			snapshot.Cursor = encodeWorkflowCursor(receipt.RunID, *ev.DurableSeq)
			snapshot.ExecutionStopped, snapshot.CanResume = false, false
			snapshot.WorkflowNodes = map[string]workflowagent.NodeRun{}
			snapshot.Interactions = map[string]workflowagent.WorkflowInteraction{}
			snapshot.Result, snapshot.ErrorCode, snapshot.FailedNode = nil, "", ""
			return projectWorkflowSnapshot(snapshot)
		}
		if *ev.DurableSeq >= sub.Handoff {
			break
		}
	}
	return workflowSnapshotDTO{}, product.NewError(product.CodeStorageUnavailable, "workflow acceptance is unavailable")
}
func decodeWorkflowReceipt(raw json.RawMessage, rid string) (workflowSnapshotDTO, error) {
	var dto workflowSnapshotDTO
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&dto) != nil || d.Decode(new(any)) != io.EOF {
		return workflowSnapshotDTO{}, product.NewError(product.CodeStorageUnavailable, "workflow creation receipt is corrupt")
	}
	seq, err := strconv.ParseUint(dto.DurableSeq, 10, 64)
	if err != nil || dto.RunID != rid || dto.Revision > (1<<53)-1 || dto.Cursor != encodeWorkflowCursor(rid, seq) {
		return workflowSnapshotDTO{}, product.NewError(product.CodeStorageUnavailable, "workflow creation receipt is corrupt")
	}
	return dto, nil
}
func (c *workflowCatalog) readOnly(ctx context.Context, rid string) (*workflowagent.WorkflowAgent, workflowagent.WorkflowSnapshot, error) {
	if storage.ValidateResourceID(rid) != nil {
		return nil, workflowagent.WorkflowSnapshot{}, product.NewError(product.CodeNotFound, "workflow run does not exist")
	}
	run, err := workflowagent.OpenWorkflowAgent(ctx, workflowagent.WorkflowOptions{StateRoot: c.root, RunID: rid, Principal: c.principal, ReadOnly: true})
	if err != nil {
		return nil, workflowagent.WorkflowSnapshot{}, err
	}
	snap, err := run.Snapshot(ctx)
	if err == nil && (snap.RunID != rid || !storage.SamePath(snap.Workspace, c.workspace)) {
		err = errors.Join(product.NewError(product.CodeNotFound, "workflow run does not exist"), errWorkflowForeignWorkspace)
	}
	if err != nil {
		_ = run.Close(context.Background())
		return nil, workflowagent.WorkflowSnapshot{}, err
	}
	return run, snap, nil
}
func (c *workflowCatalog) materialize(ctx context.Context, rid string, opts workflowagent.WorkflowOptions) (*workflowagent.WorkflowAgent, error) {
	if run := c.writers[rid]; run != nil {
		return run, nil
	}
	opts.RunID, opts.ReadOnly = rid, true
	browser, err := workflowagent.OpenWorkflowAgent(ctx, opts)
	opts.ReadOnly = false
	var run *workflowagent.WorkflowAgent
	if err == nil {
		snapshot, snapshotErr := browser.Snapshot(ctx)
		_ = browser.Close(context.Background())
		if snapshotErr != nil {
			return nil, snapshotErr
		}
		if snapshot.RunID != rid || snapshot.DefinitionName != opts.Definition.Name || snapshot.DefinitionVersion != opts.Definition.Version {
			return nil, product.NewError(product.CodeStateConflict, "workflow reservation binding differs")
		}
		run, err = workflowagent.OpenWorkflowAgent(ctx, opts)
	} else if pe, ok := product.AsError(err); ok && pe.Code == product.CodeNotFound {
		// The factory distinguishes a missing journal from an existing foreign
		// binding before creation. An empty prepared directory is retryable.
		run, err = workflowagent.CreateWorkflowAgent(ctx, opts)
	}
	if err != nil {
		return nil, err
	}
	c.writers[rid] = run
	return run, nil
}
func (c *workflowCatalog) Writer(ctx context.Context, rid string) (*workflowagent.WorkflowAgent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.usable(); err != nil {
		return nil, err
	}
	if storage.ValidateResourceID(rid) != nil {
		return nil, product.NewError(product.CodeNotFound, "workflow run does not exist")
	}
	if c.profile != codeagent.ProfileMemory {
		return nil, product.NewError(product.CodeResourceUnavailable, "workflow execution profile is unavailable")
	}
	if run := c.writers[rid]; run != nil {
		return run, nil
	}
	browser, snapshot, err := c.readOnly(ctx, rid)
	if err != nil {
		return nil, err
	}
	_ = browser.Close(context.Background())
	opts, found := c.options[snapshot.DefinitionName+"@"+snapshot.DefinitionVersion]
	if !found {
		return nil, product.NewError(product.CodeIncompatibleVersion, "workflow binding is unavailable")
	}
	return c.materialize(ctx, rid, opts)
}
func (c *workflowCatalog) Browse(ctx context.Context, rid string) (*workflowagent.WorkflowAgent, bool, func(), error) {
	c.mu.Lock()
	if err := c.usable(); err != nil {
		c.mu.Unlock()
		return nil, false, nil, err
	}
	run := c.writers[rid]
	c.mu.Unlock()
	if storage.ValidateResourceID(rid) != nil {
		return nil, false, nil, product.NewError(product.CodeNotFound, "workflow run does not exist")
	}
	if run != nil {
		return run, true, func() {}, nil
	}
	run, _, err := c.readOnly(ctx, rid)
	if err != nil {
		return nil, false, nil, err
	}
	return run, false, func() { _ = run.Close(context.Background()) }, nil
}
func (c *workflowCatalog) Snapshot(ctx context.Context, rid string) (workflowagent.WorkflowSnapshot, error) {
	run, _, release, err := c.Browse(ctx, rid)
	if err != nil {
		return workflowagent.WorkflowSnapshot{}, err
	}
	defer release()
	snap, err := run.Snapshot(ctx)
	if err == nil && snap.RunID != rid {
		return workflowagent.WorkflowSnapshot{}, product.NewError(product.CodeInternal, "workflow snapshot root differs")
	}
	return snap, err
}
func (c *workflowCatalog) Subscribe(ctx context.Context, rid string, opts workflowagent.WorkflowSubscribeOptions) (*workflowagent.WorkflowSubscription, bool, func(), error) {
	run, live, release, err := c.Browse(ctx, rid)
	if err != nil {
		return nil, false, nil, err
	}
	sub, err := run.SubscribeFrom(ctx, opts)
	if err != nil {
		release()
		return nil, false, nil, err
	}
	return sub, live, func() { sub.Close(); release() }, nil
}
func (c *workflowCatalog) List(ctx context.Context, req CatalogListRequest) (workflowListDTO, error) {
	c.mu.Lock()
	err := c.usable()
	c.mu.Unlock()
	if err != nil {
		return workflowListDTO{}, err
	}
	limit := req.Limit
	if limit <= 0 {
		limit = catalogDefaultPage
	}
	if limit > catalogMaxPage {
		return workflowListDTO{}, invalid("page size exceeds limit")
	}
	path := filepath.Join(c.root, "workflow-runs")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return workflowListDTO{Runs: []workflowEntryDTO{}}, nil
	}
	if err != nil || !info.IsDir() {
		return workflowListDTO{}, product.NewError(product.CodeStorageUnavailable, "workflow directory is unavailable")
	}
	if linked, err := storage.IsReparse(path); err != nil || linked {
		return workflowListDTO{}, product.NewError(product.CodeStorageUnavailable, "workflow directory has invalid type")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return workflowListDTO{}, product.NewError(product.CodeStorageUnavailable, "workflow directory is unavailable")
	}
	defer root.Close()
	file, err := root.Open(".")
	if err != nil {
		return workflowListDTO{}, product.NewError(product.CodeStorageUnavailable, "workflow directory is unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return workflowListDTO{}, product.NewError(product.CodeStorageUnavailable, "workflow directory changed")
	}
	entries, err := file.ReadDir(-1)
	if err != nil {
		return workflowListDTO{}, product.NewError(product.CodeStorageUnavailable, "workflow directory is unavailable")
	}
	ids := []string{}
	for _, entry := range entries {
		if entry.IsDir() && entry.Type()&os.ModeSymlink == 0 && storage.ValidateResourceID(entry.Name()) == nil && entry.Name() > req.After {
			if _, err := storage.OpenResourceDir(c.root, storage.ResourceWorkflow, entry.Name()); err == nil {
				ids = append(ids, entry.Name())
			}
		}
	}
	slices.Sort(ids)
	out := workflowListDTO{Runs: []workflowEntryDTO{}}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return workflowListDTO{}, err
		}
		snapshot, err := c.Snapshot(ctx, id)
		if pe, ok := product.AsError(err); errors.Is(err, errWorkflowForeignWorkspace) || ok && pe.Code == product.CodePermissionDenied {
			continue
		}
		if len(out.Runs) == limit {
			out.Next = out.Runs[limit-1].RunID
			break
		}
		entry := workflowEntryDTO{RunID: id}
		if err == nil {
			entry.DefinitionName, entry.DefinitionVersion, entry.State, entry.Available = snapshot.DefinitionName, snapshot.DefinitionVersion, snapshot.State, true
		}
		out.Runs = append(out.Runs, entry)
	}
	return out, nil
}
func (c *workflowCatalog) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	writers := make(map[string]*workflowagent.WorkflowAgent, len(c.writers))
	for id, run := range c.writers {
		writers[id] = run
	}
	c.mu.Unlock()
	var result error
	for id, run := range writers {
		err := run.Close(ctx)
		result = errors.Join(result, err)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			continue
		}
		c.mu.Lock()
		delete(c.writers, id)
		c.mu.Unlock()
	}
	return result
}

// resourceCatalogs is the single registry owner. Catalog.Close still owns its
// registry in standalone use; borrowed catalogs never release this shared lock.
type resourceCatalogs struct {
	code      *Catalog
	workflows *workflowCatalog
	registry  storage.CreationRegistry
}

func newResourceCatalogs(runtime runtimeOptions) (*resourceCatalogs, error) {
	code, err := NewCatalog(runtime.Code)
	if err != nil {
		return nil, err
	}
	workflows, err := newWorkflowCatalog(code.opts, runtime.Workflows, code.registry)
	if err != nil {
		_ = code.Close(context.Background())
		return nil, err
	}
	code.ownsRegistry = false
	return &resourceCatalogs{code: code, workflows: workflows, registry: code.registry}, nil
}
func (c *resourceCatalogs) Close(ctx context.Context) error {
	results := make(chan error, 2)
	go func() { results <- c.code.Close(ctx) }()
	go func() { results <- c.workflows.Close(ctx) }()
	result := errors.Join(<-results, <-results)
	c.code.mu.Lock()
	codeExited := len(c.code.writers) == 0 && c.code.activeFileWrites == 0
	c.code.mu.Unlock()
	c.workflows.mu.Lock()
	workflowExited := len(c.workflows.writers) == 0
	c.workflows.mu.Unlock()
	if codeExited && workflowExited {
		result = errors.Join(result, c.registry.Close())
	}
	return result
}
