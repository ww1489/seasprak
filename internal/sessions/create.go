package sessions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	sessstate "github.com/ww1489/seasprak/internal/sessions/state"
	sessstore "github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/jsonl"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
)

// CreateAgentSession validates the real workspace and state root, records a generation
// manifest, and starts a session. Directory modes request Unix owner-only permissions.
// They are not sandbox certification: ACLs, mounts, and hard links can still alias paths.
func CreateAgentSession(ctx context.Context, opts Options) (*AgentSession, error) {
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
	if !memoryStore {
		if _, err := os.Stat(journalPath(opts.StateRoot, opts.SessionID)); err == nil {
			return nil, product.NewError(product.CodeStateConflict, "session already exists")
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	owned := opts.Store == nil
	backend, err := openCreateStore(opts, manifest.ID, memoryStore)
	if err != nil {
		return nil, err
	}
	// Acquire the writer and validate the session path before writing resources.
	if !memoryStore {
		if err := saveManifest(opts.StateRoot, opts.SessionID, manifest); err != nil {
			if owned {
				_ = backend.Close()
			}
			return nil, err
		}
	}
	opts.Store = backend
	manager, err := sessstate.NewManager(backend, opts.SessionID)
	if err != nil {
		if owned {
			_ = backend.Close()
		}
		return nil, err
	}
	started, err := Start(opts, manager, manifest.ID)
	if err != nil {
		if owned {
			_ = backend.Close()
		}
		return nil, err
	}
	return started, nil
}

// OpenAgentSession reads an existing journal header before opening the store.
// It restores the bound real workspace and does not switch that binding.
// ReadOnly can browse when the manifest, model, or workspace directory is unavailable.
func OpenAgentSession(ctx context.Context, opts Options) (*AgentSession, error) {
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
	header, err := readHeader(opts.StateRoot, opts.SessionID, opts.Limits.WithDefaults().MaxCommitLineBytes)
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
	backend, err := jsonl.Open(opts.SessionID, opts.StateRoot, sessstore.Header{}, jsonl.Options{OpenExisting: true, MaxLine: opts.Limits.WithDefaults().MaxCommitLineBytes})
	if err != nil {
		return nil, err
	}
	manager, err := sessstate.NewManager(backend, opts.SessionID)
	if err != nil {
		_ = backend.Close()
		return nil, err
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
			_ = backend.Close()
			return nil, err
		}
	}
	if generation != "" {
		if err := matchGeneration(&opts, generation); err != nil {
			if opts.ReadOnly {
				opts.Tools = nil
				opts.ToolInfos = nil
			} else {
				_ = backend.Close()
				return nil, err
			}
		}
	} else if !opts.ReadOnly {
		_ = backend.Close()
		return nil, product.NewError(product.CodeIncompatibleVersion, "generation manifest is missing")
	}
	if !opts.ReadOnly && opts.Model == nil {
		_ = backend.Close()
		return nil, product.NewError(product.CodeInvalidArgument, "model is required")
	}
	opts.Store = backend
	if opts.Instruction == "" {
		opts.Instruction = "You are a test agent with the explicitly enabled memory tools."
	}
	started, err := Start(opts, manager, generation)
	if err != nil {
		_ = backend.Close()
		return nil, err
	}
	return started, nil
}

func openCreateStore(opts Options, generation string, memoryStore bool) (sessstore.Store, error) {
	header := sessstore.Header{Workspace: workspaceJSON(opts.Workspace, generation)}
	if opts.Store != nil {
		return opts.Store, nil
	}
	if memoryStore {
		return memory.Open(opts.SessionID, header)
	}
	return jsonl.Open(opts.SessionID, opts.StateRoot, header, jsonl.Options{MaxLine: opts.Limits.WithDefaults().MaxCommitLineBytes})
}

func readHeader(stateRoot, sessionID string, maxLine int) (sessstore.Header, error) {
	if maxLine <= 0 {
		maxLine = config.DefaultLimits().MaxCommitLineBytes
	}
	f, err := os.Open(journalPath(stateRoot, sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return sessstore.Header{}, product.NewError(product.CodeNotFound, "session does not exist")
		}
		return sessstore.Header{}, err
	}
	defer f.Close()
	line, err := readLimitedLine(f, maxLine)
	if err != nil {
		return sessstore.Header{}, err
	}
	if len(bytes.TrimSpace(line)) == 0 {
		return sessstore.Header{}, product.NewError(product.CodeNotFound, "session does not exist")
	}
	var header sessstore.Header
	if err := json.Unmarshal(bytes.TrimSpace(line), &header); err != nil || header.FormatVersion != 1 || header.SessionID != sessionID || len(header.Workspace) == 0 {
		return sessstore.Header{}, product.NewError(product.CodeIncompatibleVersion, "session header is invalid")
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
