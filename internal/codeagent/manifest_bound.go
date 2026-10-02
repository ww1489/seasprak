package codeagent

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	goruntime "runtime"

	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

// The backend owns fileRoot. A failed borrowed publication never retries its
// diagnostic path; standalone and injected callers own their checked roots.
func saveManifestForOptions(opts Options, manifest capabilityManifest) error {
	if err := store.ValidateResourceID(opts.SessionID); err != nil {
		return err
	}
	if opts.fileRoot != nil {
		return saveManifestFromRoot(opts.fileRoot, manifest, nil)
	}
	return saveManifest(opts.StateRoot, opts.SessionID, manifest)
}

// These finite, per-call boundaries belong only to manifest publication. Nil
// uses real fixed I/O. Public Create supplies no alternative publisher.
type manifestPublishBoundaries struct {
	openChild func(*os.Root, string) (*os.Root, error)
	openTemp  func(*os.Root, string) (*os.File, error)
	write     func(*os.File, []byte) (int, error)
	sync      func(*os.File) error
	close     func(*os.File) error
	rename    func(*os.Root, string, os.FileInfo) (bool, error)
	syncRoot  func(*os.Root) error
}

func saveStandaloneManifest(stateRoot, sessionID string, manifest capabilityManifest, boundary *manifestPublishBoundaries) (err error) {
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
	var openChild func(*os.Root, string) (*os.Root, error)
	if boundary != nil {
		openChild = boundary.openChild
	}
	roots, err := store.OpenResourceRoots(dir, store.ResourceCode, sessionID, true, openChild)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := roots.Close(); err == nil {
			err = closeErr
		}
	}()
	for _, root := range []*os.Root{roots.Namespace, roots.Resource} {
		if err := root.Chmod(".", 0o700); err != nil && goruntime.GOOS != "windows" {
			return err
		}
	}
	return saveManifestFromRoot(roots.Resource, manifest, boundary)
}

// saveManifestFromRoot borrows session and owns the two checked child roots.
// Sync each actual parent even for an existing child: a failed earlier sync
// must not be treated as durable merely because its mkdir already succeeded.
func saveManifestFromRoot(session *os.Root, manifest capabilityManifest, boundary *manifestPublishBoundaries) (err error) {
	if err := store.ValidateResourceID(manifest.ID); err != nil {
		return err
	}
	if session == nil {
		return os.ErrInvalid
	}
	b := manifestPublishBoundaries{}
	if boundary != nil {
		b = *boundary
	}
	if b.syncRoot == nil {
		b.syncRoot = store.SyncRoot
	}
	resources, err := store.OpenChildRoot(session, "resources", true, b.openChild)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := resources.Close(); err == nil {
			err = closeErr
		}
	}()
	if err := resources.Chmod(".", 0o700); err != nil && goruntime.GOOS != "windows" {
		return err
	}
	if err := b.syncRoot(session); err != nil {
		return err
	}
	generation, err := store.OpenChildRoot(resources, manifest.ID, true, b.openChild)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := generation.Close(); err == nil {
			err = closeErr
		}
	}()
	if err := generation.Chmod(".", 0o700); err != nil && goruntime.GOOS != "windows" {
		return err
	}
	if err := b.syncRoot(resources); err != nil {
		return err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return publishManifest(generation, raw, b)
}

func publishManifest(generation *os.Root, raw []byte, b manifestPublishBoundaries) (err error) {
	if b.openTemp == nil {
		b.openTemp = func(root *os.Root, name string) (*os.File, error) {
			return root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		}
	}
	if b.write == nil {
		b.write = (*os.File).Write
	}
	if b.sync == nil {
		b.sync = (*os.File).Sync
	}
	if b.close == nil {
		b.close = (*os.File).Close
	}
	if b.rename == nil {
		b.rename = renameManifest
	}
	if b.syncRoot == nil {
		b.syncRoot = store.SyncRoot
	}
	name := ".manifest-" + rand.Text()
	file, openErr := b.openTemp(generation, name)
	var created os.FileInfo
	if file != nil {
		// Even File+error owns the returned File. Only an actual Stat can
		// authenticate the created object for observation-based cleanup.
		created, err = file.Stat()
	}
	published := false
	defer func() {
		if file != nil {
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
		}
		if !published && manifestOrdinary(created) {
			current, statErr := generation.Lstat(name)
			if statErr == nil && manifestOrdinary(current) && os.SameFile(created, current) {
				// Size is not ownership: a partial own write is removable.
				// This observed check is not an atomic expected-ID unlink.
				_ = generation.Remove(name)
			}
		}
	}()
	if openErr != nil {
		return openErr
	}
	if err != nil {
		return err
	}
	if file == nil || !manifestOrdinary(created) {
		return manifestIdentityError()
	}
	n, err := b.write(file, raw)
	if err != nil {
		return err
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	if err := b.sync(file); err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil && goruntime.GOOS != "windows" {
		return err
	}
	written, err := file.Stat()
	if err != nil {
		return err
	}
	if !manifestOrdinary(written) || !os.SameFile(created, written) || written.Size() != int64(len(raw)) {
		return manifestIdentityError()
	}
	if err := b.close(file); err != nil {
		// A boundary may fail before closing. Always attempt actual Close.
		return err
	}
	file = nil
	current, err := generation.Lstat(name)
	if err != nil {
		return err
	}
	if !manifestWritten(current, written) {
		return manifestIdentityError()
	}
	// A successful rename cancels temp cleanup before directory sync, even
	// if post-rename verification or source/directory Close reports failure.
	published, err = b.rename(generation, name, written)
	if err != nil {
		return err
	}
	if !published {
		return manifestIdentityError()
	}
	return b.syncRoot(generation)
}

func manifestOrdinary(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && !store.IsReparseInfo(info)
}

func manifestWritten(current, written os.FileInfo) bool {
	return manifestOrdinary(current) && manifestOrdinary(written) && current.Size() == written.Size() && os.SameFile(current, written)
}

func manifestIdentityError() error {
	return product.NewError(product.CodeInvalidArgument, "manifest candidate has invalid type or identity")
}
