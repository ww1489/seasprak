// Package operations provides in-memory test fixtures, not native backends or
// sandbox certification. References and patches registered here are test-only
// data; they do not define a public SDK content or patch format.
package operations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	einofs "github.com/cloudwego/eino/adk/filesystem"
	"io"
	"sync"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

// Patch is an immutable, fixture-private exact text edit.
type Patch struct {
	Old, New   string
	ReplaceAll bool
}
type file struct {
	data    []byte
	version string
}
type content struct {
	data          []byte
	path, version string
	read          agent.ReadRequest
}
type artifact struct {
	ref  agent.ArtifactRef
	data []byte
	auth agent.AuthorizedExecution
}

// Memory shares full content between file and artifact ports. It never touches
// the host filesystem. Discovery uses logical slash-separated paths.
type Memory struct {
	mu              sync.Mutex
	environment     string
	next            int
	files           map[string]file
	contents        map[string]content
	patches         map[string]Patch
	artifacts       map[string]artifact
	outputArtifacts map[string]outputArtifact
	calls           map[string]int
}

func NewMemory() *Memory {
	return &Memory{environment: "test-memory:" + agent.MustID(), files: map[string]file{}, contents: map[string]content{}, patches: map[string]Patch{}, artifacts: map[string]artifact{}, calls: map[string]int{}}
}
func (m *Memory) ref(kind string) string {
	m.next++
	return fmt.Sprintf("%s:%s:%d", m.environment, kind, m.next)
}
func hash(data []byte) string         { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func failure(code string) error       { return product.NewError(code, "in-memory test operation rejected") }
func (m *Memory) count(method string) { m.mu.Lock(); m.calls[method]++; m.mu.Unlock() }

// Calls counts actual port invocations, including rejected ones.
func (m *Memory) Calls(method string) int { m.mu.Lock(); defer m.mu.Unlock(); return m.calls[method] }

// RegisterContent takes a detached immutable copy. A reference is never body text.
func (m *Memory) RegisterContent(data []byte) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref := m.ref("content")
	m.contents[ref] = content{data: bytes.Clone(data)}
	return ref
}
func (m *Memory) RegisterPatch(p Patch) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref := m.ref("patch")
	m.patches[ref] = p
	return ref
}

// SeedFile sets fixture state without pretending to be an authorized operation.
func (m *Memory) SeedFile(path string, data []byte) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	version := hash(data)
	m.files[path] = file{bytes.Clone(data), version}
	return version
}
func (m *Memory) Snapshot(path string) ([]byte, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[path]
	return bytes.Clone(f.data), f.version, ok
}
func (m *Memory) ExecutionCapabilities(ctx context.Context) (agent.BackendCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return agent.BackendCapabilities{}, err
	}
	return capabilities(m.environment, "file"), nil
}
func capabilities(environment, kind string) agent.BackendCapabilities {
	// "full" applies only to this isolated in-memory model, which has no host
	// filesystem/process access. This report certifies no OS security property.
	return agent.BackendCapabilities{BackendID: "test-memory-" + kind, Version: "fixture-v1", EnvironmentID: environment, SupportedModes: []string{"workspace-write"}, Enforcement: "full", RuntimeDataWriteProtected: true}
}
func (m *Memory) Read(ctx context.Context, r agent.ReadRequest) (agent.ReadResult, error) {
	m.count("read")
	if err := ctx.Err(); err != nil {
		return agent.ReadResult{}, err
	}
	if r.Offset < 0 || r.Limit < 0 || (r.Mode != "" && r.Mode != "lines" && r.Mode != "bytes") {
		return agent.ReadResult{}, failure(product.CodeInvalidArgument)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[r.Identity]
	if !ok {
		return agent.ReadResult{}, failure(product.CodeNotFound)
	}
	if r.Version != "" && r.Version != f.version {
		return agent.ReadResult{}, failure(product.CodeStateConflict)
	}
	ref := m.ref("read")
	m.contents[ref] = content{data: bytes.Clone(f.data), path: r.Identity, version: f.version, read: r}
	// Explicit modes always expose the full immutable snapshot. Open verifies
	// the original range against FrozenExecution before consuming its ticket.
	// Empty Mode alone retains the historical byte-range NextOffset metadata.
	var next int64
	if r.Mode == "" {
		next = int64(len(f.data))
		if r.Offset < next && r.Limit > 0 && r.Limit < next-r.Offset {
			next = r.Offset + r.Limit
		}
	}
	return agent.ReadResult{ContentRef: ref, Version: f.version, NextOffset: next}, nil
}

type fileArgs struct{ Operation, Path, ContentRef, PatchRef, ExpectedVersion string }

func matchesBackend(f agent.FrozenExecution, environment, kind string) bool {
	// Keep the fixture's report identical to the snapshot frozen by Executor.
	report := capabilities(environment, kind)
	raw, err := json.Marshal(report)
	return err == nil && f.SandboxMode == "workspace-write" && f.BackendCapabilitiesHash == hash(raw)
}

func (m *Memory) fileBinding(auth agent.AuthorizedExecution, operation, path, ref, version string) error {
	f := auth.Frozen
	if !matchesBackend(f, m.environment, "file") {
		return failure(product.CodePermissionDenied)
	}
	var a fileArgs
	if !auth.Ticket.Issued() || f.BackendID != "file-operations" || f.SandboxMode != "workspace-write" || json.Unmarshal(f.FinalArguments, &a) != nil {
		return failure(product.CodePermissionDenied)
	}
	if a.Operation == "" {
		switch f.Tool {
		case "write_file":
			a.Operation = "write"
		case "edit_file":
			a.Operation = "edit"
		}
	}
	actual := a.ContentRef
	if operation == "edit" {
		actual = a.PatchRef
	}
	if a.Operation != operation || a.Path != path || path == "" || actual != ref || a.ExpectedVersion != version || len(f.Resources) != 1 || f.Resources[0].Identity != "path:"+path || f.Resources[0].ExpectedVersion != version {
		return failure(product.CodePermissionDenied)
	}
	return nil
}
func (m *Memory) Write(ctx context.Context, r agent.AuthorizedFileWrite) (agent.FileEffect, error) {
	m.count("write")
	none := agent.FileEffect{SideEffect: "none"}
	if err := m.fileBinding(r.Authorization, "write", r.Path, r.ContentRef, r.ExpectedVersion); err != nil {
		return none, err
	}
	if err := r.Authorization.Validate(ctx); err != nil {
		return none, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return none, err
	}
	c, ok := m.contents[r.ContentRef]
	if !ok {
		return none, failure(product.CodeNotFound)
	}
	if r.ExpectedVersion != "" && m.files[r.Path].version != r.ExpectedVersion {
		return none, failure(product.CodeStateConflict)
	}
	return m.put(r.Path, c.data), nil
}
func (m *Memory) Edit(ctx context.Context, r agent.AuthorizedFileEdit) (agent.FileEffect, error) {
	m.count("edit")
	none := agent.FileEffect{SideEffect: "none"}
	if err := m.fileBinding(r.Authorization, "edit", r.Path, r.PatchRef, r.ExpectedVersion); err != nil {
		return none, err
	}
	var bound agent.AuthorizedFileEdit
	if json.Unmarshal(r.Authorization.Frozen.FinalArguments, &bound) != nil || bound.OldString != r.OldString || bound.NewString != r.NewString || bound.ReplaceAll != r.ReplaceAll || (r.PatchRef != "" && (r.OldString != "" || r.NewString != "" || r.ReplaceAll)) {
		return none, failure(product.CodePermissionDenied)
	}
	if err := r.Authorization.Validate(ctx); err != nil {
		return none, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return none, err
	}
	p := Patch{Old: r.OldString, New: r.NewString, ReplaceAll: r.ReplaceAll}
	if r.PatchRef != "" {
		var ok bool
		p, ok = m.patches[r.PatchRef]
		if !ok {
			return none, failure(product.CodeNotFound)
		}
	}
	f, ok := m.files[r.Path]
	if !ok {
		return none, failure(product.CodeNotFound)
	}
	if r.ExpectedVersion != "" && f.version != r.ExpectedVersion {
		return none, failure(product.CodeStateConflict)
	}
	if p.Old == "" || p.Old == p.New {
		return none, failure(product.CodeInvalidArgument)
	}
	// Reuse Eino's literal unique/replace-all implementation on a detached
	// snapshot; only our version-checked, ticket-bound put commits the effect.
	backend := einofs.NewInMemoryBackend()
	if err := backend.Write(ctx, &einofs.WriteRequest{FilePath: "edit", Content: string(f.data)}); err != nil {
		return none, failure(product.CodeResourceUnavailable)
	}
	if err := backend.Edit(ctx, &einofs.EditRequest{FilePath: "edit", OldString: p.Old, NewString: p.New, ReplaceAll: p.ReplaceAll}); err != nil {
		return none, failure(product.CodeStateConflict)
	}
	updated, err := backend.Read(ctx, &einofs.ReadRequest{FilePath: "edit"})
	if err != nil {
		return none, failure(product.CodeResourceUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return none, err
	}
	return m.put(r.Path, []byte(updated.Content)), nil
}
func (m *Memory) put(path string, data []byte) agent.FileEffect {
	version := hash(data)
	m.files[path] = file{bytes.Clone(data), version}
	return agent.FileEffect{Identity: path, Version: version, SideEffect: "confirmed", Confirmed: true}
}

func (m *Memory) Save(ctx context.Context, r agent.ArtifactInput) (agent.ArtifactRef, error) {
	m.count("save")
	f := r.Authorization.Frozen
	var a struct{ Operation, ContentRef, MediaType, Name string }
	if f.BackendID != "artifact-store" || json.Unmarshal(f.FinalArguments, &a) != nil || a.Operation != "save" || a.ContentRef != r.ContentRef || a.MediaType != r.MediaType || a.Name != r.Name {
		return agent.ArtifactRef{}, failure(product.CodePermissionDenied)
	}
	if err := r.Authorization.Validate(ctx); err != nil {
		return agent.ArtifactRef{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return agent.ArtifactRef{}, err
	}
	c, ok := m.contents[r.ContentRef]
	if !ok {
		return agent.ArtifactRef{}, failure(product.CodeNotFound)
	}
	ref := agent.ArtifactRef{ID: m.ref("artifact"), SessionID: f.Scope.SessionID, Environment: m.environment, Hash: hash(c.data), Size: int64(len(c.data)), Available: true}
	m.artifacts[ref.ID] = artifact{ref, bytes.Clone(c.data), agent.NewAuthorizedExecution(r.Authorization.Ticket, f)}
	return ref, nil
}
func (m *Memory) Open(ctx context.Context, r agent.ArtifactRead) (io.ReadCloser, error) {
	m.count("open")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.Offset < 0 || r.Limit < 0 {
		return nil, failure(product.CodeInvalidArgument)
	}
	m.mu.Lock()
	a, saved := m.artifacts[r.Ref.ID]
	c, found := m.contents[r.Ref.ID]
	m.mu.Unlock()
	var data []byte
	if saved {
		// Fixture-only result read: require the exact authority that already saved
		// this immutable artifact. This is not a second execution or a new grant.
		// A copied Save still fails one-use validation. No process-log chaining.
		digest, err := r.Authorization.Frozen.Digest()
		if err != nil || r.Ref != a.ref || r.Authorization.Ticket != a.auth.Ticket || digest != a.auth.Frozen.Hash || r.Authorization.Frozen.Hash != digest {
			return nil, failure(product.CodePermissionDenied)
		}
		data = a.data
	} else {
		if !found || c.path == "" {
			return nil, failure(product.CodeNotFound)
		}
		f := r.Authorization.Frozen
		var args struct {
			Path, Version, ExpectedVersion       string
			Offset, Limit, ByteOffset, ByteLimit *int64
		}
		if !matchesBackend(f, m.environment, "file") || f.BackendID != "file-operations" || f.Tool != "read_file" || json.Unmarshal(f.FinalArguments, &args) != nil {
			return nil, failure(product.CodePermissionDenied)
		}
		version := args.Version
		if version == "" {
			version = args.ExpectedVersion
		}
		mode, offset, limit := "lines", args.Offset, args.Limit
		if args.ByteOffset != nil || args.ByteLimit != nil {
			if offset != nil || limit != nil {
				return nil, failure(product.CodePermissionDenied)
			}
			mode, offset, limit = "bytes", args.ByteOffset, args.ByteLimit
		}
		expected := agent.ReadRequest{Identity: args.Path, Version: version, Mode: mode}
		if offset != nil {
			expected.Offset = *offset
		}
		if limit != nil {
			expected.Limit = *limit
		}
		if expected.Offset < 0 || expected.Limit < 0 || args.Path == "" || args.Path != c.path || (version != "" && version != c.version) || len(f.Resources) != 1 || f.Resources[0].Identity != "path:"+args.Path || f.Resources[0].ExpectedVersion != version {
			return nil, failure(product.CodePermissionDenied)
		}
		// Legacy snapshots cannot be substituted for an explicit-mode read_file
		// snapshot; doing so would silently restore byte slicing for line ranges.
		if c.read.Mode == "" || r.Offset != 0 || r.Limit != 0 {
			return nil, failure(product.CodePermissionDenied)
		}
		if c.read != expected {
			return nil, failure(product.CodePermissionDenied)
		}
		if err := r.Authorization.Validate(ctx); err != nil {
			return nil, err
		}
		data = c.data
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	start := min(r.Offset, int64(len(data)))
	end := int64(len(data))
	if r.Limit > 0 && r.Limit < end-start {
		end = start + r.Limit
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(data[start:end]))), nil
}

var _ agent.FileOperations = (*Memory)(nil)
var _ agent.ArtifactStore = (*Memory)(nil)
var _ agent.BackendCapabilityReporter = (*Memory)(nil)
