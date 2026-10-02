package jsonl

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

var _ store.CheckpointBlobs = (*Store)(nil)

// Put publishes a complete file with a non-replacing hard link. Unsupported
// hard links fail explicitly; there is no rename or copy fallback. A failed
// directory sync may leave an orphan, but never returns a usable reference.
func (s *Store) Put(ctx context.Context, sessionID string, data []byte) (store.BlobRef, error) {
	return s.putWithOpen(ctx, sessionID, data, nil, openBlobFile)
}

// The per-call openers enter the real relative I/O path. Public Put/Get always
// use the fixed defaults; roots are borrowed only while Store.mu is held.
func (s *Store) putWithOpen(ctx context.Context, sessionID string, data []byte, openChild func(*os.Root, string) (*os.Root, error), openFile func(*os.Root, string, int, os.FileMode) (*os.File, error)) (store.BlobRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := callContext(ctx); err != nil {
		return store.BlobRef{}, err
	}
	if err := s.writable(); err != nil {
		return store.BlobRef{}, err
	}
	if sessionID != s.sessionID {
		return store.BlobRef{}, product.NewError(product.CodeNotFound, "session mismatch")
	}
	data = bytes.Clone(data)
	ref := store.BlobRef{Hash: fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data))}
	root, err := s.blobRoot(true, openChild)
	if err != nil {
		return store.BlobRef{}, err
	}
	defer root.Close()
	name := ref.Hash + ".bin"
	if _, err := root.Lstat(name); err == nil {
		if _, err = s.readBlob(root, ref, openFile); err != nil {
			return store.BlobRef{}, err
		}
		// Re-sync even on retry: a previous Put may have failed after publication.
		if err = s.syncBlobDir(root, filepath.Join(s.dir, "checkpoints")); err != nil {
			return store.BlobRef{}, blobIO(err)
		}
		return ref, nil
	} else if !os.IsNotExist(err) {
		return store.BlobRef{}, blobIO(err)
	}
	tmp := ".checkpoint-" + rand.Text() + ".tmp"
	f, err := openFile(root, tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	var opened os.FileInfo
	var statErr error
	cleanupPending := true
	if f != nil {
		opened, statErr = f.Stat()
		defer func() {
			_ = f.Close()
			if cleanupPending {
				_ = removeOwnedBlobTemp(root, tmp, opened)
			}
		}()
	}
	if err != nil {
		return store.BlobRef{}, blobIO(err)
	}
	if statErr != nil {
		return store.BlobRef{}, blobIO(statErr)
	}
	if err = checkBlobIdentity(root, tmp, opened, 0); err != nil {
		return store.BlobRef{}, err
	}
	write := s.write
	if write == nil {
		write = func(f *os.File, p []byte) (int, error) { return f.Write(p) }
	}
	n, err := write(f, data)
	if err != nil || n != len(data) {
		return store.BlobRef{}, blobIO(io.ErrShortWrite)
	}
	if err = s.syncFile(f); err != nil {
		return store.BlobRef{}, blobIO(err)
	}
	written, err := f.Stat()
	if err != nil {
		return store.BlobRef{}, blobIO(err)
	}
	if !ordinaryBoundFile(written) || written.Size() != ref.Size || !os.SameFile(opened, written) {
		return store.BlobRef{}, blobChanged()
	}
	if err = f.Close(); err != nil {
		return store.BlobRef{}, blobIO(err)
	}
	if err = callContext(ctx); err != nil {
		return store.BlobRef{}, err
	}
	// Keep the actual written file's identity across Close, as required by
	// Windows sharing, and reject a changed temporary name before real Link.
	if err = checkBlobIdentity(root, tmp, written, ref.Size); err != nil {
		return store.BlobRef{}, err
	}
	if err = root.Link(tmp, name); err != nil {
		if !os.IsExist(err) {
			return store.BlobRef{}, blobIO(err)
		}
		if _, err = s.readBlob(root, ref, openFile); err != nil {
			return store.BlobRef{}, err
		}
	} else if err = checkBlobIdentity(root, name, written, ref.Size); err != nil {
		return store.BlobRef{}, err
	}
	if err = removeOwnedBlobTemp(root, tmp, opened); err != nil {
		return store.BlobRef{}, err
	}
	cleanupPending = false
	if err = s.syncBlobDir(root, filepath.Join(s.dir, "checkpoints")); err != nil {
		return store.BlobRef{}, blobIO(err)
	}
	return ref, nil
}

func (s *Store) Get(ctx context.Context, sessionID string, ref store.BlobRef) ([]byte, error) {
	return s.getWithOpen(ctx, sessionID, ref, nil, openBlobFile)
}

func (s *Store) getWithOpen(ctx context.Context, sessionID string, ref store.BlobRef, openChild func(*os.Root, string) (*os.Root, error), openFile func(*os.Root, string, int, os.FileMode) (*os.File, error)) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := callContext(ctx); err != nil {
		return nil, err
	}
	if err := s.readable(); err != nil {
		return nil, err
	}
	if sessionID != s.sessionID {
		return nil, product.NewError(product.CodeNotFound, "session mismatch")
	}
	if err := store.ValidateBlobRef(ref); err != nil {
		return nil, err
	}
	root, err := s.blobRoot(false, openChild)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	data, err := s.readBlob(root, ref, openFile)
	if err != nil {
		return nil, err
	}
	if err = callContext(ctx); err != nil {
		return nil, err
	}
	return data, nil
}

// blobRoot borrows the Store-owned resource and checks the journal binding
// before opening an owned checkpoints child. Diagnostic paths never select I/O.
func (s *Store) blobRoot(create bool, openChild func(*os.Root, string) (*os.Root, error)) (*os.Root, error) {
	if s.roots == nil || s.roots.Resource == nil {
		return nil, blobIO(os.ErrClosed)
	}
	session := s.roots.Resource
	bound, err := s.journal.Stat()
	if err != nil {
		return nil, blobIO(err)
	}
	current, err := session.Lstat("journal.jsonl")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, product.NewError(product.CodeStorageUnavailable, "checkpoint session binding changed")
		}
		return nil, blobIO(err)
	}
	if !ordinaryBoundFile(bound) || !ordinaryBoundFile(current) || !os.SameFile(bound, current) {
		return nil, product.NewError(product.CodeStorageUnavailable, "checkpoint session binding changed")
	}
	root, err := store.OpenChildRoot(session, "checkpoints", create, openChild)
	if err != nil {
		return nil, blobIO(err)
	}
	// Sync the parent even when the directory already exists: an earlier failed
	// attempt may have created it without acknowledging directory durability.
	if create {
		if err = s.syncBlobDir(session, s.dir); err != nil {
			root.Close()
			return nil, blobIO(err)
		}
	}
	return root, nil
}

// An explicit host hook retains its path contract. Nil selects only the actual
// borrowed directory, never a default path-based hook or a path fallback.
func (s *Store) syncBlobDir(root *os.Root, diagnostic string) error {
	if s.syncDir != nil {
		return s.syncDir(diagnostic)
	}
	return store.SyncRoot(root)
}

func openBlobFile(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
	if flags == os.O_RDONLY {
		return root.Open(name)
	}
	return root.OpenFile(name, flags, mode)
}

// removeOwnedBlobTemp checks ownership in the same checked child. Size is not
// ownership: a failed write may leave a partial file with the original identity.
// This observation is not atomic with Remove; unknown or changed names remain.
func removeOwnedBlobTemp(root *os.Root, name string, owned os.FileInfo) error {
	if !ordinaryBoundFile(owned) {
		return blobChanged()
	}
	current, err := root.Lstat(name)
	if err != nil {
		if os.IsNotExist(err) {
			return blobChanged()
		}
		return blobIO(err)
	}
	if !ordinaryBoundFile(current) || !os.SameFile(owned, current) {
		return blobChanged()
	}
	if err = root.Remove(name); err != nil {
		return blobIO(err)
	}
	return nil
}

func checkBlobIdentity(root *os.Root, name string, opened os.FileInfo, size int64) error {
	if !ordinaryBoundFile(opened) || opened.Size() != size {
		return blobChanged()
	}
	current, err := root.Lstat(name)
	if err != nil {
		if os.IsNotExist(err) {
			return blobChanged()
		}
		return blobIO(err)
	}
	if !ordinaryBoundFile(current) || current.Size() != size || !os.SameFile(opened, current) {
		return blobChanged()
	}
	return nil
}

func blobChanged() error {
	return product.NewError(product.CodeStorageUnavailable, "checkpoint changed while accessing")
}

func (s *Store) readBlob(root *os.Root, ref store.BlobRef, openFile func(*os.Root, string, int, os.FileMode) (*os.File, error)) ([]byte, error) {
	name := ref.Hash + ".bin"
	info, err := root.Lstat(name)
	if err != nil {
		return nil, blobIO(err)
	}
	if !ordinaryBoundFile(info) || info.Size() != ref.Size {
		return nil, product.NewError(product.CodeStorageUnavailable, "checkpoint integrity check failed")
	}
	f, err := openFile(root, name, os.O_RDONLY, 0)
	if err != nil {
		if f != nil {
			_ = f.Close()
		}
		if changed := checkBlobIdentity(root, name, info, ref.Size); changed != nil {
			return nil, changed
		}
		return nil, blobIO(err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, blobIO(err)
	}
	if !ordinaryBoundFile(opened) || opened.Size() != ref.Size || !os.SameFile(info, opened) {
		return nil, blobChanged()
	}
	data, err := io.ReadAll(io.LimitReader(f, ref.Size))
	if err != nil {
		return nil, blobIO(err)
	}
	var extra [1]byte
	if n, err := f.Read(extra[:]); n != 0 || err != io.EOF {
		return nil, product.NewError(product.CodeStorageUnavailable, "checkpoint length changed")
	}
	if err = store.VerifyBlob(ref, data); err != nil {
		return nil, err
	}
	if err = checkBlobIdentity(root, name, opened, ref.Size); err != nil {
		return nil, err
	}
	return data, nil
}

func blobIO(err error) error {
	if os.IsNotExist(err) {
		return product.NewError(product.CodeNotFound, "checkpoint not found")
	}
	return product.NewError(product.CodeStorageUnavailable, "checkpoint storage operation failed")
}
