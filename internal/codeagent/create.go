package codeagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/ww1489/seasprak/internal/agent"
	sessstate "github.com/ww1489/seasprak/internal/codeagent/state"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	sessstore "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/storage/memory"
)

// CreateAgentSession validates the real workspace and state root, records a generation
// manifest, and starts a session. Directory modes request Unix owner-only permissions.
// They are not sandbox certification: ACLs, mounts, and hard links can still alias paths.
func CreateAgentSession(ctx context.Context, opts Options) (*AgentSession, error) {
	return createAgentSessionWithOpen(ctx, opts, nil, jsonl.OpenBound)
}

func createAgentSessionWithOpen(ctx context.Context, opts Options, openChild func(*os.Root, string) (*os.Root, error), openBound codeBoundOpener) (out *AgentSession, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ws, stateRoot, err := resolveRoots(opts, true)
	if err != nil {
		return nil, err
	}
	opts.Workspace = ws
	opts.StateRoot = stateRoot
	if opts.SessionID == "" {
		opts.SessionID, err = agent.NewID()
		if err != nil {
			return nil, err
		}
	}
	if err := sessstore.ValidateResourceID(opts.SessionID); err != nil {
		return nil, err
	}
	if !opts.ReadOnly && opts.Model == nil {
		return nil, product.NewError(product.CodeInvalidArgument, "model is required")
	}
	decls, err := alignTools(&opts)
	if err != nil {
		return nil, err
	}
	manifest, err := buildManifest("", decls, opts.GenerationFingerprint)
	if err != nil {
		return nil, err
	}
	if opts.Instruction == "" {
		opts.Instruction = "You are a test agent with the explicitly enabled memory tools."
	}
	memoryStore := opts.Profile == ProfileMemory && opts.StateRoot == "memory"
	if !memoryStore && opts.Store != nil {
		if _, err := os.Stat(journalPath(opts.StateRoot, opts.SessionID)); err == nil {
			return nil, product.NewError(product.CodeStateConflict, "session already exists")
		} else if err != nil && !os.IsNotExist(err) {
			return nil, factoryError(err)
		}
	}
	owned := opts.Store == nil
	backend, err := openCreateStore(ctx, &opts, manifest.ID, memoryStore, openChild, openBound)
	if err != nil {
		return nil, err
	}
	defer func() {
		if owned && out == nil {
			err = closeFactoryOwners(err, backend)
		}
	}()
	// Publish through the backend's actual session root; memory saves nothing.
	if !memoryStore {
		if err := saveManifestForOptions(opts, manifest); err != nil {
			return nil, factoryError(err)
		}
	}
	opts.Store = backend
	manager, err := sessstate.NewManager(backend, opts.SessionID)
	if err != nil {
		return nil, factoryError(err)
	}
	started, err := Start(opts, manager, manifest.ID)
	if err != nil {
		return nil, factoryError(err)
	}
	return started, nil
}

// OpenAgentSession reads an existing journal header before opening the store.
// It restores the bound real workspace and does not switch that binding.
// ReadOnly can browse when the manifest, model, or workspace directory is unavailable.
func OpenAgentSession(ctx context.Context, opts Options) (*AgentSession, error) {
	return openAgentSessionWithOpen(ctx, opts, nil, jsonl.OpenBound)
}

func openAgentSessionWithOpen(ctx context.Context, opts Options, openChild func(*os.Root, string) (*os.Root, error), openBound codeBoundOpener) (out *AgentSession, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := sessstore.ValidateResourceID(opts.SessionID); err != nil {
		return nil, err
	}
	stateRoot, err := sessstore.ResolveDir(opts.StateRoot)
	if err != nil {
		return nil, err
	}
	opts.StateRoot = stateRoot
	roots, err := sessstore.OpenResourceRoots(stateRoot, sessstore.ResourceCode, opts.SessionID, false, openChild)
	if err != nil {
		return nil, headerRootError(err)
	}
	var inspected *os.File
	var backend *jsonl.Store
	defer func() {
		if inspected != nil {
			err = closeFactoryOwners(err, inspected)
		}
		if roots != nil {
			err = closeFactoryOwners(err, roots)
		}
		if out == nil && backend != nil {
			err = closeFactoryOwners(err, backend)
		}
	}()
	// Type classification precedes the native journal reader's fixed I/O.
	if _, err := checkHeaderLeaf(roots.Resource); err != nil {
		return nil, err
	}
	inspected, err = jsonl.OpenJournalReader(roots.Resource)
	if err != nil {
		return nil, headerReaderError(err)
	}
	maxLine := opts.Limits.WithDefaults().MaxCommitLineBytes
	header, err := readHeaderFromFile(roots.Resource, inspected, opts.SessionID, maxLine, product.CodeInvalidArgument)
	if err != nil {
		return nil, err
	}
	binding, err := decodeBinding(header.Workspace)
	if err != nil {
		return nil, err
	}
	if err := bindWorkspace(&opts, binding.HostRealRoot); err != nil {
		return nil, err
	}
	if !opts.ReadOnly {
		// Read the complete retained prefix before writer locks or chmod.
		if err := validateCodePrefix(inspected, opts.SessionID, maxLine, binding); err != nil {
			return nil, factoryError(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	backend, err = openBound(opts.SessionID, roots, inspected, header, jsonl.Options{OpenExisting: true, ReadOnly: opts.ReadOnly, MaxLine: maxLine})
	if err != nil {
		return nil, factoryError(err)
	}
	roots = nil // Successful OpenBound transfers both roots to the backend.
	closeErr := inspected.Close()
	inspected = nil
	if closeErr != nil {
		return nil, product.NewError(product.CodeStorageUnavailable, "session inspection close failed")
	}
	opts.fileRoot = backend.ResourceRoot()
	// The final retained journal must still have the inspected workspace
	// binding, even if its bytes were changed without replacing its inode.
	stored, err := backend.Load(ctx, opts.SessionID)
	if err != nil {
		return nil, factoryError(err)
	}
	finalBinding, err := decodeBinding(stored.Header.Workspace)
	if err != nil {
		return nil, err
	}
	if finalBinding != binding {
		return nil, product.NewError(product.CodeIncompatibleVersion, "workspace binding changed while opening")
	}
	manager, err := sessstate.NewManager(backend, opts.SessionID)
	if err != nil {
		return nil, factoryError(err)
	}
	view := manager.View()
	generation := view.Generation
	if generation == "" {
		generation = binding.Generation
	}
	if err := sessstore.ValidateResourceID(generation); err != nil {
		if opts.ReadOnly {
			opts.Tools = nil
			opts.ToolInfos = nil
			generation = ""
		} else {
			return nil, err
		}
	}
	if generation != "" {
		if err := matchGeneration(&opts, generation); err != nil {
			if opts.ReadOnly {
				opts.Tools = nil
				opts.ToolInfos = nil
			} else {
				return nil, factoryError(err)
			}
		}
	} else if !opts.ReadOnly {
		return nil, product.NewError(product.CodeIncompatibleVersion, "generation manifest is missing")
	}
	if !opts.ReadOnly && opts.Model == nil {
		return nil, product.NewError(product.CodeInvalidArgument, "model is required")
	}
	opts.Store = backend
	if opts.Instruction == "" {
		opts.Instruction = "You are a test agent with the explicitly enabled memory tools."
	}
	started, err := Start(opts, manager, generation)
	if err != nil {
		return nil, factoryError(err)
	}
	return started, nil
}

func openCreateStore(ctx context.Context, opts *Options, generation string, memoryStore bool, openChild func(*os.Root, string) (*os.Root, error), openBound codeBoundOpener) (sessstore.Store, error) {
	header := sessstore.Header{Workspace: workspaceJSON(opts.Workspace, generation)}
	if opts.Store != nil {
		stored, err := opts.Store.Load(context.Background(), opts.SessionID)
		if err != nil {
			return nil, err
		}
		// Preserve the legacy bare injected-store contract, but never allow a
		// typed Workflow journal to become a Code session writer.
		if stored.Header.ResourceType != "" || stored.Header.RunID != "" || stored.Header.SessionID != "" {
			if err := sessstore.ValidateHeader(stored.Header, sessstore.ResourceCode, opts.SessionID); err != nil {
				return nil, err
			}
		}
		var binding workspaceBinding
		decoded := json.Unmarshal(stored.Header.Workspace, &binding)
		if stored.Header.ResourceType == sessstore.ResourceCode || binding.HostRealRoot != "" {
			if decoded != nil || binding.HostRealRoot == "" || !sessstore.SamePath(binding.HostRealRoot, opts.Workspace) {
				return nil, product.NewError(product.CodeIncompatibleVersion, "injected Code workspace binding is incompatible")
			}
		}
		return opts.Store, nil
	}
	if memoryStore {
		return memory.Open(opts.SessionID, header)
	}
	backend, err := openCreateFileStore(ctx, *opts, header, openChild, openBound)
	if err != nil {
		return nil, err
	}
	opts.fileRoot = backend.ResourceRoot()
	return backend, nil
}

func readHeader(stateRoot, sessionID string, maxLine int) (sessstore.Header, error) {
	return readHeaderWithLimitCode(stateRoot, sessionID, maxLine, product.CodeInvalidArgument)
}

func readHeaderWithLimitCode(stateRoot, sessionID string, maxLine int, limitCode string) (sessstore.Header, error) {
	return readHeaderWithOpenRoot(stateRoot, sessionID, maxLine, limitCode, nil)
}

// The opener is passed per call at the checked relative child-open boundary.
// Public entry points use the real parent.OpenRoot through a nil opener.
func readHeaderWithOpenRoot(stateRoot, sessionID string, maxLine int, limitCode string, openChild func(*os.Root, string) (*os.Root, error)) (header sessstore.Header, err error) {
	roots, err := sessstore.OpenResourceRoots(stateRoot, sessstore.ResourceCode, sessionID, false, openChild)
	if err != nil {
		return sessstore.Header{}, headerRootError(err)
	}
	defer func() { err = closeFactoryOwners(err, roots) }()
	return readHeaderFromRoot(roots.Resource, sessionID, maxLine, limitCode)
}

// readHeaderFromRoot borrows the already checked session root; its owner closes it.
func readHeaderFromRoot(root *os.Root, sessionID string, maxLine int, limitCode string) (header sessstore.Header, err error) {
	info, err := checkHeaderLeaf(root)
	if err != nil {
		return sessstore.Header{}, err
	}
	f, err := root.Open("journal.jsonl")
	if err != nil {
		return sessstore.Header{}, product.NewError(product.CodeStorageUnavailable, "session header is unavailable")
	}
	defer func() { err = closeFactoryOwners(err, f) }()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || sessstore.IsReparseInfo(opened) || !os.SameFile(info, opened) {
		return sessstore.Header{}, product.NewError(product.CodeIncompatibleVersion, "session header changed while opening")
	}
	return readHeaderFromFile(root, f, sessionID, maxLine, limitCode)
}

// readHeaderFromFile borrows both the checked root and the exact retained file.
// The bounded SectionReader leaves the caller's inspection offset unchanged.
func readHeaderFromFile(root *os.Root, f *os.File, sessionID string, maxLine int, limitCode string) (sessstore.Header, error) {
	if maxLine <= 0 {
		maxLine = config.DefaultLimits().MaxCommitLineBytes
	}
	info, err := root.Lstat("journal.jsonl")
	if err != nil || !info.Mode().IsRegular() || sessstore.IsReparseInfo(info) {
		return sessstore.Header{}, product.NewError(product.CodeIncompatibleVersion, "session header changed while opening")
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || sessstore.IsReparseInfo(opened) || !os.SameFile(info, opened) {
		return sessstore.Header{}, product.NewError(product.CodeIncompatibleVersion, "session header changed while opening")
	}
	line, err := readLimitedLine(io.NewSectionReader(f, 0, opened.Size()), maxLine)
	if err != nil {
		if pe, ok := product.AsError(err); ok && pe.Code == product.CodeInvalidArgument {
			return sessstore.Header{}, product.NewError(limitCode, "session header exceeds limit")
		}
		return sessstore.Header{}, product.NewError(product.CodeStorageUnavailable, "session header is unavailable")
	}
	current, err := root.Lstat("journal.jsonl")
	if err != nil || !current.Mode().IsRegular() || sessstore.IsReparseInfo(current) || !os.SameFile(opened, current) {
		return sessstore.Header{}, product.NewError(product.CodeIncompatibleVersion, "session header changed while reading")
	}
	if len(bytes.TrimSpace(line)) == 0 {
		return sessstore.Header{}, product.NewError(product.CodeIncompatibleVersion, "session header is invalid")
	}
	var header sessstore.Header
	if err := json.Unmarshal(bytes.TrimSpace(line), &header); err != nil || header.FormatVersion != 1 || header.SessionID != sessionID || len(header.Workspace) == 0 {
		return sessstore.Header{}, product.NewError(product.CodeIncompatibleVersion, "session header is invalid")
	}
	if err := sessstore.ValidateHeader(header, sessstore.ResourceCode, sessionID); err != nil {
		return sessstore.Header{}, err
	}
	return header, nil
}

func readLimitedLine(r io.Reader, maxLine int) ([]byte, error) {
	br := bufio.NewReader(r)
	var buf []byte
	for {
		frag, err := br.ReadSlice('\n')
		buf = append(buf, frag...)
		if len(buf) > maxLine+1 {
			return nil, product.NewError(product.CodeInvalidArgument, "journal line exceeds limit")
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
		return buf, nil
	}
}
