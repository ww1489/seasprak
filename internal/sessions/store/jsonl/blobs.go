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
	"github.com/ww1489/seasprak/internal/sessions/store"
)

var _ store.CheckpointBlobs = (*Store)(nil)

// Put publishes a complete file with a non-replacing hard link. Unsupported
// hard links fail explicitly; there is no rename or copy fallback. A failed
// directory sync may leave an orphan, but never returns a usable reference.
func (s *Store) Put(ctx context.Context, sessionID string, data []byte) (store.BlobRef, error) {
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
	root, err := s.blobRoot(true)
	if err != nil {
		return store.BlobRef{}, err
	}
	defer root.Close()
	name := ref.Hash + ".bin"
	if _, err := root.Lstat(name); err == nil {
		if _, err = s.readBlob(root, ref); err != nil {
			return store.BlobRef{}, err
		}
		// Re-sync even on retry: a previous Put may have failed after publication.
		if err = s.syncDir(filepath.Join(s.dir, "checkpoints")); err != nil {
			return store.BlobRef{}, blobIO(err)
		}
		return ref, nil
	} else if !os.IsNotExist(err) {
		return store.BlobRef{}, blobIO(err)
	}
	tmp := ".checkpoint-" + rand.Text() + ".tmp"
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return store.BlobRef{}, blobIO(err)
	}
	defer root.Remove(tmp)
	defer f.Close()
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
	if err = f.Close(); err != nil {
		return store.BlobRef{}, blobIO(err)
	}
	if err = callContext(ctx); err != nil {
		return store.BlobRef{}, err
	}
	if err = root.Link(tmp, name); err != nil {
		if !os.IsExist(err) {
			return store.BlobRef{}, blobIO(err)
		}
		if _, err = s.readBlob(root, ref); err != nil {
			return store.BlobRef{}, err
		}
	}
	if err = root.Remove(tmp); err != nil {
		return store.BlobRef{}, blobIO(err)
	}
	if err = s.syncDir(filepath.Join(s.dir, "checkpoints")); err != nil {
		return store.BlobRef{}, blobIO(err)
	}
	return ref, nil
}

func (s *Store) Get(ctx context.Context, sessionID string, ref store.BlobRef) ([]byte, error) {
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
	root, err := s.blobRoot(false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	data, err := s.readBlob(root, ref)
	if err != nil {
		return nil, err
	}
	if err = callContext(ctx); err != nil {
		return nil, err
	}
	return data, nil
}

// blobRoot checks the existing binding and directory metadata without chmod.
// Root constrains subsequent access; these checks are not OS sandbox certification.
func (s *Store) blobRoot(create bool) (*os.Root, error) {
	for _, path := range []string{filepath.Dir(s.dir), s.dir} {
		if err := blobPath(path, true); err != nil {
			return nil, err
		}
	}
	session, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, blobIO(err)
	}
	defer session.Close()
	bound, err := s.journal.Stat()
	if err != nil {
		return nil, blobIO(err)
	}
	current, err := session.Stat("journal.jsonl")
	if err != nil {
		return nil, blobIO(err)
	}
	if !os.SameFile(bound, current) {
		return nil, product.NewError(product.CodeStorageUnavailable, "checkpoint session binding changed")
	}
	path := filepath.Join(s.dir, "checkpoints")
	if create {
		if err = session.Mkdir("checkpoints", 0700); err != nil && !os.IsExist(err) {
			return nil, blobIO(err)
		}
	}
	if err = blobPath(path, true); err != nil {
		return nil, err
	}
	root, err := session.OpenRoot("checkpoints")
	if err != nil {
		return nil, blobIO(err)
	}
	// Sync the parent even when the directory already exists: an earlier failed
	// attempt may have created it without acknowledging directory durability.
	if create {
		if err = s.syncDir(s.dir); err != nil {
			root.Close()
			return nil, blobIO(err)
		}
	}
	return root, nil
}

func blobPath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return blobIO(err)
	}
	reparse, err := store.IsReparse(path)
	if err != nil {
		return blobIO(err)
	}
	if reparse || directory && !info.IsDir() || !directory && !info.Mode().IsRegular() {
		return product.NewError(product.CodeStorageUnavailable, "checkpoint path has invalid file type")
	}
	return nil
}

func (s *Store) readBlob(root *os.Root, ref store.BlobRef) ([]byte, error) {
	name := ref.Hash + ".bin"
	if err := blobPath(filepath.Join(s.dir, "checkpoints", name), false); err != nil {
		return nil, err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return nil, blobIO(err)
	}
	if !info.Mode().IsRegular() || info.Size() != ref.Size {
		return nil, product.NewError(product.CodeStorageUnavailable, "checkpoint integrity check failed")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, blobIO(err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, blobIO(err)
	}
	if !opened.Mode().IsRegular() || opened.Size() != ref.Size || !os.SameFile(info, opened) {
		return nil, product.NewError(product.CodeStorageUnavailable, "checkpoint changed while opening")
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
	return data, nil
}

func blobIO(err error) error {
	if os.IsNotExist(err) {
		return product.NewError(product.CodeNotFound, "checkpoint not found")
	}
	return product.NewError(product.CodeStorageUnavailable, "checkpoint storage operation failed")
}
