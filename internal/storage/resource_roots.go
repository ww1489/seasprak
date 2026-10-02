package storage

import (
	"os"
	"path/filepath"
	"strings"

	product "github.com/ww1489/seasprak/internal/errors"
)

// ResourceRoots owns the checked namespace and resource directories. Path is
// diagnostic information only; bound I/O must use Namespace or Resource.
// The state root and its ancestors remain a trusted deployment boundary.
type ResourceRoots struct {
	Namespace *os.Root
	Resource  *os.Root
	Path      string
}

// Close attempts both owned roots in resource-to-namespace order. Root.Close
// is idempotent, but does not report every underlying syscall close failure.
func (r *ResourceRoots) Close() error {
	if r == nil {
		return nil
	}
	var err error
	if r.Resource != nil {
		err = r.Resource.Close()
	}
	if r.Namespace != nil {
		if closeErr := r.Namespace.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

// OpenResourceRoots opens the trusted state root once and checks each finite
// child relative to its parent. Existing directories are never chmodded here.
func OpenResourceRoots(stateRoot string, kind ResourceType, id string, create bool, openChild func(*os.Root, string) (*os.Root, error)) (*ResourceRoots, error) {
	return openResourceRoots(stateRoot, kind, id, create, openChild, nil)
}

// The optional sync boundary is private and per call. Public factories always
// use SyncRoot on the same checked parent, never a reopened diagnostic path.
func openResourceRoots(stateRoot string, kind ResourceType, id string, create bool, openChild func(*os.Root, string) (*os.Root, error), syncParent func(*os.Root) error) (*ResourceRoots, error) {
	namespace, err := resourceNamespace(kind)
	if err != nil {
		return nil, err
	}
	if err := ValidateResourceID(id); err != nil {
		return nil, err
	}
	if strings.TrimSpace(stateRoot) == "" {
		return nil, product.NewError(product.CodeInvalidArgument, "state root is required")
	}
	path, err := filepath.Abs(stateRoot)
	if err != nil {
		return nil, err
	}
	// Preserve trusted-root configuration errors; this metadata check is not
	// an atomic root authentication or a basis for subsequent child I/O.
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return nil, product.NewError(product.CodeInvalidArgument, "state root must be an existing directory")
	}
	state, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer state.Close()
	if syncParent == nil {
		syncParent = SyncRoot
	}
	ns, err := OpenChildRoot(state, namespace, create, openChild)
	if err != nil {
		return nil, err
	}
	if create {
		if err := syncParent(state); err != nil {
			_ = ns.Close()
			return nil, err
		}
	}
	resource, err := OpenChildRoot(ns, id, create, openChild)
	if err != nil {
		_ = ns.Close()
		return nil, err
	}
	if create {
		if err := syncParent(ns); err != nil {
			_ = resource.Close()
			_ = ns.Close()
			return nil, err
		}
	}
	return &ResourceRoots{Namespace: ns, Resource: resource, Path: filepath.Join(path, namespace, id)}, nil
}

// OpenChildRoot checks before/opened/current identity and observed reparse
// metadata. The optional opener is a per-call boundary before the real relative
// open, not a filesystem abstraction. Root containment does not atomically
// reject every transient in-root link, mount or hard-link alias.
func OpenChildRoot(parent *os.Root, name string, create bool, openChild func(*os.Root, string) (*os.Root, error)) (*os.Root, error) {
	if err := ValidateSessionID(name); err != nil {
		return nil, err
	}
	before, err := parent.Lstat(name)
	if os.IsNotExist(err) && create {
		if err := parent.Mkdir(name, 0o700); err != nil && !os.IsExist(err) {
			return nil, err
		}
		before, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || IsReparseInfo(before) {
		return nil, product.NewError(product.CodeInvalidArgument, "resource path is not an ordinary directory")
	}
	if openChild == nil {
		openChild = (*os.Root).OpenRoot
	}
	child, err := openChild(parent, name)
	if err != nil {
		if child != nil {
			_ = child.Close()
		}
		// A containment failure may be caused by a replacement link. Preserve
		// ordinary I/O errors only when the checked name still has its identity.
		current, currentErr := parent.Lstat(name)
		if os.IsNotExist(currentErr) || currentErr == nil && (!current.IsDir() || IsReparseInfo(current) || !os.SameFile(before, current)) {
			return nil, product.NewError(product.CodeInvalidArgument, "resource directory changed while opening")
		}
		return nil, err
	}
	opened, err := child.Stat(".")
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	current, err := parent.Lstat(name)
	if err != nil {
		_ = child.Close()
		if os.IsNotExist(err) {
			return nil, product.NewError(product.CodeInvalidArgument, "resource directory changed while opening")
		}
		return nil, err
	}
	if !opened.IsDir() || IsReparseInfo(opened) || !current.IsDir() || IsReparseInfo(current) || !os.SameFile(before, opened) || !os.SameFile(opened, current) {
		_ = child.Close()
		return nil, product.NewError(product.CodeInvalidArgument, "resource directory changed while opening")
	}
	return child, nil
}
