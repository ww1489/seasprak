package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"

	product "github.com/ww1489/seasprak/internal/errors"
	sessstore "github.com/ww1489/seasprak/internal/sessions/store"
)

type workspaceBinding struct {
	HostRealRoot string `json:"hostRealRoot"`
	Generation   string `json:"generation,omitempty"`
}

func resolveRoots(opts Options, creating bool) (string, string, error) {
	if !creating && opts.ReadOnly && opts.Workspace == "" {
		return "", "", product.NewError(product.CodeInvalidArgument, "workspace is required")
	}
	if opts.Workspace == "" && creating {
		return "", "", product.NewError(product.CodeInvalidArgument, "workspace is required")
	}
	ws, wsErr := sessstore.ResolveDir(opts.Workspace)
	if wsErr != nil {
		if opts.ReadOnly && !creating {
			return opts.Workspace, "", nil
		}
		if creating {
			return "", "", product.NewError(product.CodeInvalidArgument, "workspace must be an existing directory")
		}
		return "", "", wsErr
	}
	if opts.Profile == ProfileMemory && opts.StateRoot == "memory" {
		return ws, "memory", nil
	}
	stateRoot := opts.StateRoot
	if stateRoot == "" {
		var err error
		stateRoot, err = defaultStateRoot()
		if err != nil {
			return "", "", err
		}
	}
	realState, err := sessstore.ResolveDir(stateRoot)
	if err != nil {
		return "", "", err
	}
	if sessstore.PathsOverlap(ws, realState) {
		return "", "", product.NewError(product.CodeInvalidArgument, "state root overlaps the workspace")
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

func bindWorkspace(opts *Options, bound string) error {
	real, err := sessstore.ResolveDir(bound)
	if err != nil {
		if !opts.ReadOnly {
			return product.NewError(product.CodeInvalidArgument, "bound workspace is unavailable")
		}
		if opts.Workspace != "" && opts.Workspace != bound {
			return product.NewError(product.CodeStateConflict, "workspace binding does not match")
		}
		opts.Workspace = bound
		return nil
	}
	if opts.Workspace != "" {
		got, err := sessstore.ResolveDir(opts.Workspace)
		if err != nil || !sessstore.SamePath(got, real) {
			return product.NewError(product.CodeStateConflict, "workspace binding does not match")
		}
	}
	opts.Workspace = real
	return nil
}

func decodeBinding(raw json.RawMessage) (workspaceBinding, error) {
	var binding workspaceBinding
	if err := json.Unmarshal(raw, &binding); err != nil || binding.HostRealRoot == "" {
		return workspaceBinding{}, product.NewError(product.CodeIncompatibleVersion, "workspace binding is invalid")
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
