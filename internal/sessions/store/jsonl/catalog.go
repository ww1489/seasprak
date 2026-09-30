package jsonl

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/gofrs/flock"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

type creationRegistry struct {
	mu     sync.Mutex
	path   string
	root   *os.Root
	lock   *flock.Flock
	closed bool
}
type creationEnvelope struct {
	Version int                  `json:"version"`
	Key     string               `json:"key"`
	Record  store.CreationRecord `json:"record"`
}

func OpenCreationRegistry(stateRoot string) (store.CreationRegistry, error) {
	path, err := store.ResolveDir(stateRoot)
	if err != nil {
		return nil, err
	}
	for _, part := range []string{"catalog", "creations"} {
		parent := path
		path = filepath.Join(path, part)
		if err = os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return nil, creationIO()
		}
		linked, e := store.IsReparse(path)
		if e != nil || linked {
			return nil, creationIO()
		}
		info, e := os.Lstat(path)
		if e != nil || !info.IsDir() {
			return nil, creationIO()
		}
		if err = store.SyncDir(parent); err != nil {
			return nil, creationIO()
		}
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, creationIO()
	}
	lockPath := filepath.Join(path, "writer.lock")
	if info, e := os.Lstat(lockPath); e == nil && !info.Mode().IsRegular() {
		root.Close()
		return nil, creationIO()
	} else if e != nil && !os.IsNotExist(e) {
		root.Close()
		return nil, creationIO()
	}
	lk := flock.New(lockPath)
	ok, err := lk.TryLock()
	if err != nil || !ok {
		root.Close()
		if err != nil {
			return nil, creationIO()
		}
		return nil, product.NewError(product.CodeStateConflict, "session catalog already has a writer")
	}
	return &creationRegistry{path: path, root: root, lock: lk}, nil
}
func creationKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
func creationIO() error {
	return product.NewError(product.CodeStorageUnavailable, "session creation registry is unavailable")
}
func (r *creationRegistry) load(key string) (store.CreationRecord, bool, error) {
	name := creationKey(key) + ".json"
	info, err := r.root.Lstat(name)
	if os.IsNotExist(err) {
		return store.CreationRecord{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return store.CreationRecord{}, false, creationIO()
	}
	f, err := r.root.Open(name)
	if err != nil {
		return store.CreationRecord{}, false, creationIO()
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, int64(config.DefaultLimits().MaxCommitLineBytes)+1))
	if err != nil || len(raw) > config.DefaultLimits().MaxCommitLineBytes {
		return store.CreationRecord{}, false, creationIO()
	}
	var env creationEnvelope
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&env) != nil || env.Version != 1 || env.Key != creationKey(key) || store.ValidateResourceID(env.Record.SessionID) != nil || env.Record.Digest == "" || env.Record.Generation == "" {
		return store.CreationRecord{}, false, creationIO()
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return store.CreationRecord{}, false, creationIO()
	}
	return env.Record, true, nil
}
func (r *creationRegistry) Load(ctx context.Context, key string) (store.CreationRecord, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return store.CreationRecord{}, false, err
	}
	if r.closed {
		return store.CreationRecord{}, false, creationIO()
	}
	return r.load(key)
}
func (r *creationRegistry) Save(ctx context.Context, key string, record store.CreationRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.closed {
		return creationIO()
	}
	if store.ValidateResourceID(record.SessionID) != nil || record.Digest == "" || record.Generation == "" || key == "" {
		return product.NewError(product.CodeInvalidArgument, "invalid creation record")
	}
	old, found, err := r.load(key)
	if err != nil {
		return err
	}
	if found && (old.SessionID != record.SessionID || old.Digest != record.Digest || old.Generation != record.Generation || len(old.Receipt) > 0 && !bytes.Equal(old.Receipt, record.Receipt)) {
		return product.NewError(product.CodeStateConflict, "creation record is immutable")
	}
	raw, err := json.Marshal(creationEnvelope{1, creationKey(key), record})
	if err != nil || len(raw) > config.DefaultLimits().MaxCommitLineBytes {
		return creationIO()
	}
	tmp := ".creation-" + rand.Text() + ".tmp"
	f, err := r.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return creationIO()
	}
	defer r.root.Remove(tmp)
	n, writeErr := f.Write(raw)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || n != len(raw) || syncErr != nil || closeErr != nil {
		return creationIO()
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = store.ReplaceFile(filepath.Join(r.path, tmp), filepath.Join(r.path, creationKey(key)+".json")); err != nil {
		return creationIO()
	}
	if err = store.SyncDir(r.path); err != nil {
		return creationIO()
	}
	return nil
}
func (r *creationRegistry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	closeErr := r.root.Close()
	unlockErr := r.lock.Unlock()
	if closeErr != nil || unlockErr != nil {
		return creationIO()
	}
	return nil
}
