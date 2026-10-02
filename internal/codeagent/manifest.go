package codeagent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	goruntime "runtime"
	"sort"

	"github.com/ww1489/seasprak/internal/agent/tools"

	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

const builtinVersion = "builtin-v1"

type toolDecl struct {
	Name           string                      `json:"name"`
	Version        string                      `json:"version"`
	Description    string                      `json:"description"`
	Schema         json.RawMessage             `json:"schema"`
	ToolInterface  string                      `json:"toolInterface,omitempty"`
	OutputCallback bool                        `json:"outputCallback,omitempty"`
	Execution      *tools.ExecutionDescription `json:"execution,omitempty"`
}

type capabilityManifest struct {
	ID      string     `json:"id"`
	Version string     `json:"version,omitempty"`
	Tools   []toolDecl `json:"tools"`
	Hash    string     `json:"hash"`
	Runtime string     `json:"runtime"`
	Model   string     `json:"model,omitempty"`
}

func runtimeFingerprint() string { return goruntime.Version() }

func buildManifest(id string, tools []toolDecl, modelFingerprint string) (capabilityManifest, error) {
	canon, err := canonicalTools(tools)
	if err != nil {
		return capabilityManifest{}, err
	}
	version := ""
	if len(canon) == 0 {
		version = builtinVersion
	}
	sum, err := fingerprint(version, runtimeFingerprint(), modelFingerprint, canon)
	if err != nil {
		return capabilityManifest{}, err
	}
	hash := hex.EncodeToString(sum[:])
	if id == "" {
		if len(canon) == 0 && modelFingerprint == "" {
			id = builtinVersion
		} else {
			id = hash
		}
	}
	if err := store.ValidateResourceID(id); err != nil {
		return capabilityManifest{}, err
	}
	return capabilityManifest{ID: id, Version: version, Tools: canon, Hash: hash, Runtime: runtimeFingerprint(), Model: modelFingerprint}, nil
}

func saveManifest(stateRoot, sessionID string, manifest capabilityManifest) error {
	return saveStandaloneManifest(stateRoot, sessionID, manifest, nil)
}

// loadManifestForOptions keeps runtime reads on its borrowed backend binding.
// Memory and injected callers without that binding retain the standalone path.
func loadManifestForOptions(opts Options, id string) (capabilityManifest, error) {
	if opts.fileRoot != nil {
		return loadManifestFromRoot(opts.fileRoot, id)
	}
	return loadManifest(opts.StateRoot, opts.SessionID, id)
}

func loadManifest(stateRoot, sessionID, id string) (capabilityManifest, error) {
	if err := store.ValidateResourceID(sessionID); err != nil {
		return capabilityManifest{}, err
	}
	if err := store.ValidateResourceID(id); err != nil {
		return capabilityManifest{}, err
	}
	roots, err := store.OpenResourceRoots(stateRoot, store.ResourceCode, sessionID, false, nil)
	if err != nil {
		return capabilityManifest{}, loadManifestReadError(err)
	}
	defer roots.Close()
	return loadManifestFromRoot(roots.Resource, id)
}

// loadManifestFromRoot borrows the session root and owns only the opened children.
func loadManifestFromRoot(session *os.Root, id string) (capabilityManifest, error) {
	return loadManifestFromRootWithOpen(session, id, nil, nil)
}

// The optional openers are per-call checked child/leaf boundaries for tests.
// Production callers always use the real relative opens through nil openers.
func loadManifestFromRootWithOpen(session *os.Root, id string, openChild func(*os.Root, string) (*os.Root, error), openFile func(*os.Root, string) (*os.File, error)) (capabilityManifest, error) {
	if err := store.ValidateResourceID(id); err != nil {
		return capabilityManifest{}, err
	}
	resources, err := store.OpenChildRoot(session, "resources", false, openChild)
	if err != nil {
		return capabilityManifest{}, loadManifestReadError(err)
	}
	defer resources.Close()
	generation, err := store.OpenChildRoot(resources, id, false, openChild)
	if err != nil {
		return capabilityManifest{}, loadManifestReadError(err)
	}
	defer generation.Close()
	info, err := generation.Lstat("manifest.json")
	if err != nil {
		return capabilityManifest{}, loadManifestReadError(err)
	}
	if !info.Mode().IsRegular() || store.IsReparseInfo(info) {
		return capabilityManifest{}, product.NewError(product.CodeIncompatibleVersion, "generation manifest is unreadable")
	}
	if openFile == nil {
		openFile = (*os.Root).Open
	}
	file, err := openFile(generation, "manifest.json")
	if err != nil {
		if file != nil {
			_ = file.Close()
		}
		return capabilityManifest{}, loadManifestReadError(err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || store.IsReparseInfo(opened) || !os.SameFile(info, opened) {
		return capabilityManifest{}, product.NewError(product.CodeIncompatibleVersion, "generation manifest is unreadable")
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return capabilityManifest{}, product.NewError(product.CodeIncompatibleVersion, "generation manifest is unreadable")
	}
	current, err := generation.Lstat("manifest.json")
	if err != nil || !current.Mode().IsRegular() || store.IsReparseInfo(current) || !os.SameFile(opened, current) {
		return capabilityManifest{}, product.NewError(product.CodeIncompatibleVersion, "generation manifest is unreadable")
	}
	var manifest capabilityManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return capabilityManifest{}, product.NewError(product.CodeIncompatibleVersion, "generation manifest is unreadable")
	}
	if manifest.ID != id {
		return capabilityManifest{}, product.NewError(product.CodeIncompatibleVersion, "generation manifest does not match")
	}
	sum, err := fingerprint(manifest.Version, manifest.Runtime, manifest.Model, manifest.Tools)
	if err != nil {
		return capabilityManifest{}, product.NewError(product.CodeIncompatibleVersion, "generation manifest is unreadable")
	}
	if manifest.Hash == "" || manifest.Hash != hex.EncodeToString(sum[:]) {
		return capabilityManifest{}, product.NewError(product.CodeIncompatibleVersion, "generation manifest hash does not match")
	}
	return manifest, nil
}

func loadManifestReadError(err error) error {
	if os.IsNotExist(err) {
		return product.NewError(product.CodeIncompatibleVersion, "generation manifest is missing")
	}
	return product.NewError(product.CodeIncompatibleVersion, "generation manifest is unreadable")
}

func canonicalTools(declarations []toolDecl) ([]toolDecl, error) {
	out := make([]toolDecl, 0, len(declarations))
	seen := map[string]struct{}{}
	for _, tool := range declarations {
		if tool.Name == "" || tool.Version == "" {
			return nil, product.NewError(product.CodeInvalidArgument, "tool name and implementation version are required")
		}
		if _, ok := seen[tool.Name]; ok {
			return nil, product.Errorf(product.CodeInvalidArgument, "duplicate tool %s", tool.Name)
		}
		seen[tool.Name] = struct{}{}
		schema, err := canonicalJSON(tool.Schema)
		if err != nil {
			return nil, product.NewError(product.CodeInvalidArgument, "tool schema is invalid")
		}
		if !tools.ValidToolInterface(tool.ToolInterface) {
			return nil, product.NewError(product.CodeInvalidArgument, "tool interface is unsupported")
		}
		kind := tool.ToolInterface
		if kind == "invokable" {
			kind = "" // Existing manifests omitted the original invokable interface.
		}
		decl := toolDecl{Name: tool.Name, Version: tool.Version, Description: tool.Description, Schema: json.RawMessage(schema), ToolInterface: kind, OutputCallback: tool.OutputCallback}
		if tool.Execution != nil {
			copy := tool.Execution.Clone()
			decl.Execution = &copy
		}
		out = append(out, decl)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		if out[i].Description != out[j].Description {
			return out[i].Description < out[j].Description
		}
		return string(out[i].Schema) < string(out[j].Schema)
	})
	return out, nil
}

func fingerprint(version, runtimeFP, modelFP string, tools []toolDecl) ([32]byte, error) {
	canon, err := canonicalTools(tools)
	if err != nil {
		return [32]byte{}, err
	}
	body := struct {
		Model   string     `json:"model,omitempty"`
		Runtime string     `json:"runtime"`
		Tools   []toolDecl `json:"tools"`
		Version string     `json:"version,omitempty"`
	}{Model: modelFP, Runtime: runtimeFP, Tools: canon, Version: version}
	raw, err := json.Marshal(body)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}
