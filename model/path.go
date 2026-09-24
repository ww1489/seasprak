package model

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
)

// ResolveDir returns a cleaned absolute directory after evaluating symlinks and
// junctions. A successful result means directory metadata was readable and the
// path is a directory. Unix mode bits, Windows ACLs, mounts, and hard links are
// outside this check. It is not sandbox certification.
func ResolveDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", NewError(CodeInvalidArgument, "directory path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", NewError(CodeInvalidArgument, "directory is unavailable")
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", NewError(CodeInvalidArgument, "directory is unavailable")
	}
	if !info.IsDir() {
		return "", NewError(CodeInvalidArgument, "path is not a directory")
	}
	return filepath.Clean(real), nil
}

// PathsOverlap reports whether the two cleaned paths are the same directory or
// one contains the other. Callers must pass paths from ResolveDir. On Windows
// the comparison is case-insensitive. It does not detect hard links or mounts.
func PathsOverlap(a, b string) bool {
	a, b = normPath(a), normPath(b)
	if a == b {
		return true
	}
	sep := string(os.PathSeparator)
	return strings.HasPrefix(a, b+sep) || strings.HasPrefix(b, a+sep)
}

// SamePath reports whether two cleaned paths name the same directory.
func SamePath(a, b string) bool {
	return normPath(a) == normPath(b)
}

func normPath(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

// ValidateResourceID allows a single safe segment such as sess-reopen or a hex
// id. It rejects empty values, separators, "..", colons, and Windows device names.
func ValidateResourceID(id string) error {
	if id == "" || len(id) > 128 || id != strings.TrimSpace(id) {
		return NewError(CodeInvalidArgument, "resource id must be one safe path segment")
	}
	if id == "." || id == ".." || strings.Contains(id, "..") || strings.ContainsAny(id, `/\`) || strings.Contains(id, ":") {
		return NewError(CodeInvalidArgument, "resource id must be one safe path segment")
	}
	if strings.HasSuffix(id, ".") || strings.HasSuffix(id, " ") {
		return NewError(CodeInvalidArgument, "resource id must be one safe path segment")
	}
	if filepath.Base(id) != id {
		return NewError(CodeInvalidArgument, "resource id must be one safe path segment")
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f || unicode.IsSpace(r) {
			return NewError(CodeInvalidArgument, "resource id must be one safe path segment")
		}
	}
	base := id
	if dot := strings.IndexByte(id, '.'); dot >= 0 {
		base = id[:dot]
	}
	switch strings.ToUpper(base) {
	case "CON", "PRN", "AUX", "NUL":
		return NewError(CodeInvalidArgument, "resource id uses a reserved device name")
	}
	upper := strings.ToUpper(base)
	if len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) && upper[3] >= '1' && upper[3] <= '9' {
		return NewError(CodeInvalidArgument, "resource id uses a reserved device name")
	}
	return nil
}
