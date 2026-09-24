package sessions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

const builtinVersion = "builtin-v1"

type toolDecl struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
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
	if err := store.ValidateResourceID(sessionID); err != nil {
		return err
	}
	if err := store.ValidateResourceID(manifest.ID); err != nil {
		return err
	}
	dir, err := store.ResolveDir(stateRoot)
	if err != nil {
		return err
	}
	for _, part := range []string{"sessions", sessionID, "resources", manifest.ID} {
		dir = filepath.Join(dir, part)
		if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		if linked, err := store.IsReparse(dir); err != nil {
			return err
		} else if linked {
			return product.NewError(product.CodeInvalidArgument, "resource directory is a symlink or junction")
		}
		real, err := store.ResolveDir(dir)
		if err != nil || !store.SamePath(real, dir) {
			return product.NewError(product.CodeInvalidArgument, "resource directory is a symlink or junction")
		}
		if err := os.Chmod(dir, 0o700); err != nil && goruntime.GOOS != "windows" {
			return err
		}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return writeManifestSync(filepath.Join(dir, "manifest.json"), raw)
}

func loadManifest(stateRoot, sessionID, id string) (capabilityManifest, error) {
	if err := store.ValidateResourceID(sessionID); err != nil {
		return capabilityManifest{}, err
	}
	if err := store.ValidateResourceID(id); err != nil {
		return capabilityManifest{}, err
	}
	raw, err := os.ReadFile(filepath.Join(stateRoot, "sessions", sessionID, "resources", id, "manifest.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return capabilityManifest{}, product.NewError(product.CodeIncompatibleVersion, "generation manifest is missing")
		}
		return capabilityManifest{}, err
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

func canonicalTools(tools []toolDecl) ([]toolDecl, error) {
	out := make([]toolDecl, 0, len(tools))
	seen := map[string]struct{}{}
	for _, tool := range tools {
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
		out = append(out, toolDecl{Name: tool.Name, Version: tool.Version, Description: tool.Description, Schema: json.RawMessage(schema)})
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

func writeManifestSync(path string, raw []byte) error {
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
	if err := os.Chmod(tmp.Name(), 0o600); err != nil && goruntime.GOOS != "windows" {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	ok = true
	if err := os.Chmod(path, 0o600); err != nil && goruntime.GOOS != "windows" {
		return err
	}
	return syncManifestDir(dir)
}

func syncManifestDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	err = f.Sync()
	if err != nil && goruntime.GOOS == "windows" {
		return nil
	}
	return err
}
