package storage

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ww1489/seasprak/model"
)

// ValidateSessionID accepts one relative path segment.
// Callers select stateRoot; this only rejects identifiers that can escape it.
func ValidateSessionID(id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsRune(id, 0) || strings.ContainsAny(id, `/\:`) || filepath.IsAbs(id) || filepath.VolumeName(id) != "" || filepath.Clean(id) != id {
		return model.NewError(model.CodeInvalidArgument, "session id must be a single path segment")
	}
	return nil
}

// OpenSessionDir resolves an existing sessions/<id> directory and does not create it.
func OpenSessionDir(stateRoot, sessionID string) (string, error) {
	return sessionDir(stateRoot, sessionID, false)
}

// PrepareSessionDir creates sessions/<id> under an existing state root.
// The root itself is not created. Symlinks and Windows junctions on the
// sessions directory or the session directory are rejected, and the resolved
// session path must stay inside the resolved root.
func PrepareSessionDir(stateRoot, sessionID string) (string, error) {
	return sessionDir(stateRoot, sessionID, true)
}

func sessionDir(stateRoot, sessionID string, create bool) (string, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return "", err
	}
	if stateRoot == "" {
		return "", model.NewError(model.CodeInvalidArgument, "state root is required")
	}
	root, err := filepath.Abs(stateRoot)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", model.NewError(model.CodeInvalidArgument, "state root must be an existing directory")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	sessions := filepath.Join(realRoot, "sessions")
	if err := ensureDir(sessions, create); err != nil {
		return "", err
	}
	dir := filepath.Join(sessions, sessionID)
	if err := ensureDir(dir, create); err != nil {
		return "", err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realRoot, realDir)
	want := filepath.Join("sessions", sessionID)
	if err != nil || !filepath.IsLocal(rel) || !samePath(rel, want) {
		return "", model.NewError(model.CodeInvalidArgument, "session directory escapes the state root")
	}
	return dir, nil
}

func ensureDir(path string, create bool) error {
	info, err := os.Lstat(path)
	if err == nil {
		reparse, rerr := IsReparse(path)
		if rerr != nil {
			return rerr
		}
		if reparse {
			return model.NewError(model.CodeInvalidArgument, "session path is a symlink or junction")
		}
		if !info.IsDir() {
			return model.NewError(model.CodeInvalidArgument, "session path is not a directory")
		}
		return os.Chmod(path, 0o700)
	}
	if !os.IsNotExist(err) {
		return err
	}
	if !create {
		return model.NewError(model.CodeNotFound, "session does not exist")
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func samePath(got, want string) bool {
	got, want = filepath.Clean(got), filepath.Clean(want)
	if got == want {
		return true
	}
	return runtime.GOOS == "windows" && strings.EqualFold(got, want)
}
