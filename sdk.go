package seasprak

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/cloudwego/eino/schema"
	einojson "github.com/eino-contrib/jsonschema"

	"github.com/ww1489/seasprak/agent/tools"
	"github.com/ww1489/seasprak/extensions"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/model"
	"github.com/ww1489/seasprak/session"
	"github.com/ww1489/seasprak/session/history"
)

type SessionOptions = session.Options

type workspaceBinding struct {
	HostRealRoot string `json:"hostRealRoot"`
	Generation   string `json:"generation,omitempty"`
}

// CreateAgentSession validates the real workspace and state root, records a generation
// manifest, and starts a session. Directory modes request Unix owner-only permissions.
// They are not sandbox certification: ACLs, mounts, and hard links can still alias paths.
func CreateAgentSession(ctx context.Context, opts session.Options) (*session.AgentSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ws, state, err := resolveRoots(opts, true)
	if err != nil {
		return nil, err
	}
	opts.Workspace = ws
	opts.StateRoot = state
	if opts.SessionID == "" {
		opts.SessionID, err = model.NewID()
		if err != nil {
			return nil, err
		}
	}
	if err := model.ValidateResourceID(opts.SessionID); err != nil {
		return nil, err
	}
	if !opts.ReadOnly && opts.Model == nil {
		return nil, model.NewError(model.CodeInvalidArgument, "model is required")
	}
	decls, err := alignTools(&opts)
	if err != nil {
		return nil, err
	}
	manifest, err := extensions.Build("", decls, opts.GenerationFingerprint)
	if err != nil {
		return nil, err
	}
	if opts.Instruction == "" {
		opts.Instruction = "You are a test agent with the explicitly enabled memory tools."
	}
	memoryStore := opts.Profile == session.ProfileMemory && opts.StateRoot == "memory"
	if !memoryStore {
		if _, err := os.Stat(journalPath(opts.StateRoot, opts.SessionID)); err == nil {
			return nil, model.NewError(model.CodeStateConflict, "session already exists")
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	owned := opts.Store == nil
	store, err := openCreateStore(opts, manifest.ID, memoryStore)
	if err != nil {
		return nil, err
	}
	// Acquire the writer and validate the session path before writing resources.
	if !memoryStore {
		if err := extensions.Save(opts.StateRoot, opts.SessionID, manifest); err != nil {
			if owned {
				_ = store.Close()
			}
			return nil, err
		}
	}
	opts.Store = store
	manager, err := history.NewManager(store, opts.SessionID)
	if err != nil {
		if owned {
			_ = store.Close()
		}
		return nil, err
	}
	started, err := session.Start(opts, manager, manifest.ID)
	if err != nil {
		if owned {
			_ = store.Close()
		}
		return nil, err
	}
	return started, nil
}

// OpenAgentSession reads an existing journal header before opening the store.
// It restores the bound real workspace and does not switch that binding.
// ReadOnly can browse when the manifest, model, or workspace directory is unavailable.
func OpenAgentSession(ctx context.Context, opts session.Options) (*session.AgentSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := model.ValidateResourceID(opts.SessionID); err != nil {
		return nil, err
	}
	state, err := model.ResolveDir(opts.StateRoot)
	if err != nil {
		return nil, err
	}
	opts.StateRoot = state
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
	store, err := jsonl.Open(opts.SessionID, opts.StateRoot, history.Header{}, jsonl.Options{OpenExisting: true, MaxLine: opts.Limits.WithDefaults().MaxCommitLineBytes})
	if err != nil {
		return nil, err
	}
	manager, err := history.NewManager(store, opts.SessionID)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	view := manager.View()
	generation := view.Generation
	if generation == "" {
		generation = binding.Generation
	}
	if err := model.ValidateResourceID(generation); err != nil {
		if opts.ReadOnly {
			opts.Tools = nil
			opts.ToolInfos = nil
			generation = ""
		} else {
			_ = store.Close()
			return nil, err
		}
	}
	if generation != "" {
		if err := matchGeneration(&opts, generation); err != nil {
			if opts.ReadOnly {
				opts.Tools = nil
				opts.ToolInfos = nil
			} else {
				_ = store.Close()
				return nil, err
			}
		}
	} else if !opts.ReadOnly {
		_ = store.Close()
		return nil, model.NewError(model.CodeIncompatibleVersion, "generation manifest is missing")
	}
	if !opts.ReadOnly && opts.Model == nil {
		_ = store.Close()
		return nil, model.NewError(model.CodeInvalidArgument, "model is required")
	}
	opts.Store = store
	if opts.Instruction == "" {
		opts.Instruction = "You are a test agent with the explicitly enabled memory tools."
	}
	started, err := session.Start(opts, manager, generation)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return started, nil
}

func ToolInfo(name, desc string) *schema.ToolInfo {
	return &schema.ToolInfo{
		Name: name,
		Desc: desc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"n": {Type: schema.Integer, Required: true},
		}),
	}
}

func resolveRoots(opts session.Options, creating bool) (string, string, error) {
	if !creating && opts.ReadOnly && opts.Workspace == "" {
		return "", "", model.NewError(model.CodeInvalidArgument, "workspace is required")
	}
	if opts.Workspace == "" && creating {
		return "", "", model.NewError(model.CodeInvalidArgument, "workspace is required")
	}
	ws, wsErr := model.ResolveDir(opts.Workspace)
	if wsErr != nil {
		if opts.ReadOnly && !creating {
			return opts.Workspace, "", nil
		}
		if creating {
			return "", "", model.NewError(model.CodeInvalidArgument, "workspace must be an existing directory")
		}
		return "", "", wsErr
	}
	if opts.Profile == session.ProfileMemory && opts.StateRoot == "memory" {
		return ws, "memory", nil
	}
	state := opts.StateRoot
	if state == "" {
		var err error
		state, err = defaultStateRoot()
		if err != nil {
			return "", "", err
		}
	}
	realState, err := model.ResolveDir(state)
	if err != nil {
		return "", "", err
	}
	if model.PathsOverlap(ws, realState) {
		return "", "", model.NewError(model.CodeInvalidArgument, "state root overlaps the workspace")
	}
	return ws, realState, nil
}

func defaultStateRoot() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(cfg, "seasprak", "state")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(root, 0o700); err != nil && filepath.Separator != '\\' {
		return "", err
	}
	return root, nil
}

func openCreateStore(opts session.Options, generation string, memoryStore bool) (history.SessionStore, error) {
	header := history.Header{Workspace: workspaceJSON(opts.Workspace, generation)}
	if opts.Store != nil {
		return opts.Store, nil
	}
	if memoryStore {
		return memory.Open(opts.SessionID, header)
	}
	return jsonl.Open(opts.SessionID, opts.StateRoot, header, jsonl.Options{MaxLine: opts.Limits.WithDefaults().MaxCommitLineBytes})
}

func alignTools(opts *session.Options) ([]extensions.ToolDecl, error) {
	if len(opts.Tools) == 0 {
		if len(opts.ToolInfos) != 0 {
			return nil, model.NewError(model.CodeInvalidArgument, "tool info does not match tool definitions")
		}
		opts.ToolInfos = nil
		return nil, nil
	}
	decls := make([]extensions.ToolDecl, 0, len(opts.Tools))
	infos := make([]*schema.ToolInfo, 0, len(opts.Tools))
	seen := map[string]struct{}{}
	for _, def := range opts.Tools {
		if def.Name == "" || def.Version == "" {
			return nil, model.NewError(model.CodeInvalidArgument, "tool name and implementation version are required")
		}
		if _, ok := seen[def.Name]; ok {
			return nil, model.Errorf(model.CodeInvalidArgument, "duplicate tool %s", def.Name)
		}
		seen[def.Name] = struct{}{}
		info, err := toolInfoFromDefinition(def)
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
		schemaText, err := model.CanonicalJSON(def.Schema)
		if err != nil {
			return nil, model.NewError(model.CodeInvalidArgument, "tool schema is invalid")
		}
		decls = append(decls, extensions.ToolDecl{
			Name: def.Name, Version: def.Version, Description: def.Description, Schema: json.RawMessage(schemaText),
		})
	}
	if err := matchToolInfos(decls, opts.ToolInfos); err != nil {
		return nil, err
	}
	opts.ToolInfos = infos
	return decls, nil
}

func toolInfoFromDefinition(def tools.Definition) (*schema.ToolInfo, error) {
	var js einojson.Schema
	if err := json.Unmarshal(def.Schema, &js); err != nil {
		return nil, model.NewError(model.CodeInvalidArgument, "tool schema is invalid")
	}
	return &schema.ToolInfo{Name: def.Name, Desc: def.Description, ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&js)}, nil
}

func matchToolInfos(decls []extensions.ToolDecl, infos []*schema.ToolInfo) error {
	if len(infos) == 0 {
		return nil
	}
	if len(infos) != len(decls) {
		return model.NewError(model.CodeInvalidArgument, "tool info does not match tool definitions")
	}
	byName := map[string]struct{ schema, desc string }{}
	for _, info := range infos {
		if info == nil {
			return model.NewError(model.CodeInvalidArgument, "tool info does not match tool definitions")
		}
		text, err := infoSchema(info)
		if err != nil {
			return err
		}
		if _, ok := byName[info.Name]; ok {
			return model.NewError(model.CodeInvalidArgument, "tool info does not match tool definitions")
		}
		byName[info.Name] = struct{ schema, desc string }{text, info.Desc}
	}
	for _, decl := range decls {
		got, ok := byName[decl.Name]
		if !ok || got.schema != string(decl.Schema) || got.desc != decl.Description {
			return model.NewError(model.CodeInvalidArgument, "tool info does not match tool definitions")
		}
	}
	return nil
}

func infoSchema(info *schema.ToolInfo) (string, error) {
	if info.ParamsOneOf == nil {
		return "", model.NewError(model.CodeInvalidArgument, "tool info schema is missing")
	}
	js, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(js)
	if err != nil {
		return "", err
	}
	return model.CanonicalJSON(raw)
}

func matchGeneration(opts *session.Options, generation string) error {
	loaded, err := extensions.Load(opts.StateRoot, opts.SessionID, generation)
	if err != nil {
		return err
	}
	decls, err := alignTools(opts)
	if err != nil {
		return err
	}
	current, err := extensions.Build(loaded.ID, decls, opts.GenerationFingerprint)
	if err != nil {
		return err
	}
	if loaded.Runtime != current.Runtime {
		return model.NewError(model.CodeIncompatibleResume, "runtime build fingerprint does not match the generation")
	}
	if loaded.Hash != current.Hash || loaded.Model != opts.GenerationFingerprint {
		return model.NewError(model.CodeIncompatibleVersion, "tool generation does not match the saved manifest")
	}
	return nil
}

func bindWorkspace(opts *session.Options, bound string) error {
	real, err := model.ResolveDir(bound)
	if err != nil {
		if !opts.ReadOnly {
			return model.NewError(model.CodeInvalidArgument, "bound workspace is unavailable")
		}
		if opts.Workspace != "" && opts.Workspace != bound {
			return model.NewError(model.CodeStateConflict, "workspace binding does not match")
		}
		opts.Workspace = bound
		return nil
	}
	if opts.Workspace != "" {
		got, err := model.ResolveDir(opts.Workspace)
		if err != nil || !model.SamePath(got, real) {
			return model.NewError(model.CodeStateConflict, "workspace binding does not match")
		}
	}
	opts.Workspace = real
	return nil
}

func readHeader(stateRoot, sessionID string, maxLine int) (history.Header, error) {
	if maxLine <= 0 {
		maxLine = model.DefaultLimits().MaxCommitLineBytes
	}
	f, err := os.Open(journalPath(stateRoot, sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return history.Header{}, model.NewError(model.CodeNotFound, "session does not exist")
		}
		return history.Header{}, err
	}
	defer f.Close()
	line, err := readLimitedLine(f, maxLine)
	if err != nil {
		return history.Header{}, err
	}
	if len(bytes.TrimSpace(line)) == 0 {
		return history.Header{}, model.NewError(model.CodeNotFound, "session does not exist")
	}
	var header history.Header
	if err := json.Unmarshal(bytes.TrimSpace(line), &header); err != nil || header.FormatVersion != 1 || header.SessionID != sessionID || len(header.Workspace) == 0 {
		return history.Header{}, model.NewError(model.CodeIncompatibleVersion, "session header is invalid")
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
			return nil, model.NewError(model.CodeInvalidArgument, "journal line exceeds limit")
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

func decodeBinding(raw json.RawMessage) (workspaceBinding, error) {
	var binding workspaceBinding
	if err := json.Unmarshal(raw, &binding); err != nil || binding.HostRealRoot == "" {
		return workspaceBinding{}, model.NewError(model.CodeIncompatibleVersion, "workspace binding is invalid")
	}
	return binding, nil
}

func journalPath(stateRoot, sessionID string) string {
	return filepath.Join(stateRoot, "sessions", sessionID, "journal.jsonl")
}

func workspaceJSON(root, generation string) json.RawMessage {
	raw, _ := json.Marshal(workspaceBinding{HostRealRoot: root, Generation: generation})
	return raw
}
