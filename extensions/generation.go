package extensions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/model"
)

const BuiltinVersion = "builtin-v1"

// ToolDecl is the declared tool identity. Compatibility uses this explicit
// version plus schema and description, not a function pointer.
type ToolDecl struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// Manifest is the immutable capability version selected when an input is accepted.
type Manifest struct {
	ID      string     `json:"id"`
	Version string     `json:"version,omitempty"`
	Tools   []ToolDecl `json:"tools"`
	Hash    string     `json:"hash"`
	Runtime string     `json:"runtime"`
	Model   string     `json:"model,omitempty"`
}

// RuntimeFingerprint is the current Go runtime build. It does not reconstruct tool code.
func RuntimeFingerprint() string { return runtime.Version() }

func Build(id string, tools []ToolDecl, modelFingerprint string) (Manifest, error) {
	canon, err := canonicalTools(tools)
	if err != nil {
		return Manifest{}, err
	}
	version := ""
	if len(canon) == 0 {
		version = BuiltinVersion
	}
	sum, err := fingerprint(version, RuntimeFingerprint(), modelFingerprint, canon)
	if err != nil {
		return Manifest{}, err
	}
	hash := hex.EncodeToString(sum[:])
	if id == "" {
		if len(canon) == 0 && modelFingerprint == "" {
			id = BuiltinVersion
		} else {
			id = hash
		}
	}
	if err := model.ValidateResourceID(id); err != nil {
		return Manifest{}, err
	}
	return Manifest{ID: id, Version: version, Tools: canon, Hash: hash, Runtime: RuntimeFingerprint(), Model: modelFingerprint}, nil
}

func Save(stateRoot, sessionID string, manifest Manifest) error {
	if err := model.ValidateResourceID(sessionID); err != nil {
		return err
	}
	if err := model.ValidateResourceID(manifest.ID); err != nil {
		return err
	}
	dir, err := model.ResolveDir(stateRoot)
	if err != nil {
		return err
	}
	for _, part := range []string{"sessions", sessionID, "resources", manifest.ID} {
		dir = filepath.Join(dir, part)
		if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		if linked, err := storage.IsReparse(dir); err != nil {
			return err
		} else if linked {
			return model.NewError(model.CodeInvalidArgument, "resource directory is a symlink or junction")
		}
		real, err := model.ResolveDir(dir)
		if err != nil || !model.SamePath(real, dir) {
			return model.NewError(model.CodeInvalidArgument, "resource directory is a symlink or junction")
		}
		if err := os.Chmod(dir, 0o700); err != nil && runtime.GOOS != "windows" {
			return err
		}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return writeSync(filepath.Join(dir, "manifest.json"), raw)
}

func Load(stateRoot, sessionID, id string) (Manifest, error) {
	if err := model.ValidateResourceID(sessionID); err != nil {
		return Manifest{}, err
	}
	if err := model.ValidateResourceID(id); err != nil {
		return Manifest{}, err
	}
	raw, err := os.ReadFile(filepath.Join(stateRoot, "sessions", sessionID, "resources", id, "manifest.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return Manifest{}, model.NewError(model.CodeIncompatibleVersion, "generation manifest is missing")
		}
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, model.NewError(model.CodeIncompatibleVersion, "generation manifest is unreadable")
	}
	if manifest.ID != id {
		return Manifest{}, model.NewError(model.CodeIncompatibleVersion, "generation manifest does not match")
	}
	sum, err := fingerprint(manifest.Version, manifest.Runtime, manifest.Model, manifest.Tools)
	if err != nil {
		return Manifest{}, model.NewError(model.CodeIncompatibleVersion, "generation manifest is unreadable")
	}
	if manifest.Hash == "" || manifest.Hash != hex.EncodeToString(sum[:]) {
		return Manifest{}, model.NewError(model.CodeIncompatibleVersion, "generation manifest hash does not match")
	}
	return manifest, nil
}

func canonicalTools(tools []ToolDecl) ([]ToolDecl, error) {
	out := make([]ToolDecl, 0, len(tools))
	seen := map[string]struct{}{}
	for _, tool := range tools {
		if tool.Name == "" || tool.Version == "" {
			return nil, model.NewError(model.CodeInvalidArgument, "tool name and implementation version are required")
		}
		if _, ok := seen[tool.Name]; ok {
			return nil, model.Errorf(model.CodeInvalidArgument, "duplicate tool %s", tool.Name)
		}
		seen[tool.Name] = struct{}{}
		schema, err := model.CanonicalJSON(tool.Schema)
		if err != nil {
			return nil, model.NewError(model.CodeInvalidArgument, "tool schema is invalid")
		}
		out = append(out, ToolDecl{Name: tool.Name, Version: tool.Version, Description: tool.Description, Schema: json.RawMessage(schema)})
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

func fingerprint(version, runtimeFP, modelFP string, tools []ToolDecl) ([32]byte, error) {
	canon, err := canonicalTools(tools)
	if err != nil {
		return [32]byte{}, err
	}
	body := struct {
		Model   string     `json:"model,omitempty"`
		Runtime string     `json:"runtime"`
		Tools   []ToolDecl `json:"tools"`
		Version string     `json:"version,omitempty"`
	}{Model: modelFP, Runtime: runtimeFP, Tools: canon, Version: version}
	raw, err := json.Marshal(body)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

func writeSync(path string, raw []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".manifest-*")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil && runtime.GOOS != "windows" {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	ok = true
	if err := os.Chmod(path, 0o600); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	err = f.Sync()
	if err != nil && runtime.GOOS == "windows" {
		return nil
	}
	return err
}
