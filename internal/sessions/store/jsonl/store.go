package jsonl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"github.com/ww1489/seasprak/internal/sessions/store"
)

const formatVersion = 1

// Store appends one validated JSON object per commit and syncs before acknowledging it.
// Creating or replacing a file also syncs the directory when the OS allows it.
// Windows directory flush often cannot be requested; see store.SyncDir.
// File.Sync and MoveFileEx WRITE_THROUGH do not promise durability on every filesystem.
type Store struct {
	mu        sync.Mutex
	dir       string
	sessionID string
	journal   *os.File
	lock      *flock.Flock
	maxLine   int
	syncFile  func(*os.File) error
	syncDir   func(string) error
	write     func(*os.File, []byte) (int, error)
	broken    bool
	repair    bool
	closed    bool
	header    store.Header
	chain     *store.Chain
}

type Options struct {
	MaxLine  int
	SyncFile func(*os.File) error
	SyncDir  func(string) error
	// Write replaces File.Write when tests need a short write. Nil uses File.Write.
	Write func(*os.File, []byte) (int, error)
	// OpenExisting opens a journal that is already on disk.
	// It does not create the session directory or the journal file.
	OpenExisting bool
}

func Open(sessionID, stateRoot string, header store.Header, opt Options) (*Store, error) {
	if sessionID == "" || stateRoot == "" {
		return nil, product.NewError(product.CodeInvalidArgument, "session and state root are required")
	}
	var dir string
	var err error
	if opt.OpenExisting {
		dir, err = store.OpenSessionDir(stateRoot, sessionID)
	} else {
		dir, err = store.PrepareSessionDir(stateRoot, sessionID)
	}
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "journal.jsonl")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, product.NewError(product.CodeInvalidArgument, "journal must be a regular file, not a link")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if opt.OpenExisting {
		if _, err := os.Lstat(path); err != nil {
			if os.IsNotExist(err) {
				return nil, product.NewError(product.CodeNotFound, "journal does not exist")
			}
			return nil, err
		}
	}
	lk := flock.New(filepath.Join(dir, "writer.lock"))
	ok, err := lk.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, product.NewError(product.CodeStateConflict, "session already has a writer")
	}
	flags := os.O_RDWR
	if !opt.OpenExisting {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		_ = lk.Unlock()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = f.Close()
		_ = lk.Unlock()
		return nil, err
	}
	s := &Store{
		dir: dir, sessionID: sessionID, journal: f, lock: lk,
		maxLine: opt.MaxLine, syncFile: opt.SyncFile, syncDir: opt.SyncDir, write: opt.Write,
		header: header, chain: store.NewChain(),
	}
	if s.maxLine == 0 {
		s.maxLine = config.DefaultLimits().MaxCommitLineBytes
	}
	if s.syncFile == nil {
		s.syncFile = func(file *os.File) error { return file.Sync() }
	}
	if s.syncDir == nil {
		s.syncDir = store.SyncDir
	}
	info, err := f.Stat()
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	if info.Size() == 0 {
		if opt.OpenExisting {
			_ = s.Close()
			return nil, product.NewError(product.CodeNotFound, "journal does not exist")
		}
		header.RecordType = "header"
		header.FormatVersion = formatVersion
		header.SessionID = sessionID
		if header.CreatedAt.IsZero() {
			header.CreatedAt = time.Now().UTC()
		}
		s.header = header
		if err := s.writeLine(header); err != nil {
			_ = s.Close()
			return nil, err
		}
		if err := s.syncFile(s.journal); err != nil {
			_ = s.Close()
			return nil, err
		}
		if err := s.syncDir(dir); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	if _, err := s.reload(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Load(ctx context.Context, sessionID string) (store.StoredSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := callContext(ctx); err != nil {
		return store.StoredSession{}, err
	}
	if err := s.readable(); err != nil {
		return store.StoredSession{}, err
	}
	if sessionID != s.sessionID {
		return store.StoredSession{}, product.NewError(product.CodeNotFound, "session mismatch")
	}
	return s.reload()
}

func (s *Store) reload() (store.StoredSession, error) {
	if _, err := s.journal.Seek(0, io.SeekStart); err != nil {
		return store.StoredSession{}, err
	}
	stored, err := ReadFile(s.journal, s.sessionID, s.maxLine)
	if err != nil {
		return store.StoredSession{}, err
	}
	chain := store.NewChain()
	for _, commit := range stored.Commits {
		if err := chain.Apply(commit); err != nil {
			return store.StoredSession{}, err
		}
	}
	s.chain = chain
	s.header = stored.Header
	s.repair = stored.RepairRequired
	return store.CloneSession(stored), nil
}

func (s *Store) Append(ctx context.Context, sessionID string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := callContext(ctx); err != nil {
		return store.CommitReceipt{}, err
	}
	if err := s.writable(); err != nil {
		return store.CommitReceipt{}, err
	}
	if sessionID != s.sessionID {
		return store.CommitReceipt{}, product.NewError(product.CodeNotFound, "session mismatch")
	}
	sealed, receipt, duplicate, err := s.chain.Prepare(expected, commit)
	if err != nil || duplicate {
		return receipt, err
	}
	if _, err := s.journal.Seek(0, io.SeekEnd); err != nil {
		return store.CommitReceipt{}, err
	}
	if err := s.writeLine(sealed); err != nil {
		var pe *product.Error
		if errors.As(err, &pe) && pe.Code == product.CodeInvalidArgument {
			return store.CommitReceipt{}, err
		}
		s.broken = true
		s.repair = true
		return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, err.Error())
	}
	if err := s.syncFile(s.journal); err != nil {
		s.broken = true
		return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "sync failed")
	}
	if err := s.chain.Apply(sealed); err != nil {
		s.broken = true
		return store.CommitReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) ReadAfter(ctx context.Context, sessionID string, after uint64) (store.CommitReader, error) {
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
	stored, err := s.reload()
	if err != nil {
		return nil, err
	}
	return store.ReadCommits(stored.Commits, after), nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	if s.journal != nil {
		err = s.journal.Close()
		s.journal = nil
	}
	if s.lock != nil {
		if unlockErr := s.lock.Unlock(); err == nil {
			err = unlockErr
		}
		s.lock = nil
	}
	return err
}

func callContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func (s *Store) readable() error {
	if s.closed || s.journal == nil {
		return product.NewError(product.CodeStateConflict, "session store is closed")
	}
	return nil
}

func (s *Store) writable() error {
	if err := s.readable(); err != nil {
		return err
	}
	if s.broken || s.repair {
		return product.NewError(product.CodeStorageUnavailable, "journal cannot accept writes")
	}
	return nil
}

func (s *Store) writeLine(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if bytes.Contains(raw, []byte{'\n'}) || len(raw)+1 > s.maxLine {
		return product.NewError(product.CodeInvalidArgument, "commit line exceeds limit")
	}
	payload := append(raw, '\n')
	write := s.write
	if write == nil {
		write = func(file *os.File, p []byte) (int, error) { return file.Write(p) }
	}
	n, err := write(s.journal, payload)
	if n != len(payload) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	return err
}

func Repair(path string, maxLine int) (string, error) {
	dir := filepath.Dir(path)
	lk := flock.New(filepath.Join(dir, "writer.lock"))
	ok, err := lk.TryLock()
	if err != nil {
		return "", err
	}
	if !ok {
		return "", product.NewError(product.CodeStateConflict, "session already has a writer")
	}
	defer lk.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	stored, err := ReadFile(bytes.NewReader(raw), "", maxLine)
	if err != nil {
		return "", err
	}
	if !stored.RepairRequired {
		return path, nil
	}
	candidate := path + ".repairing"
	if err := writeCandidate(candidate, stored, maxLine); err != nil {
		_ = os.Remove(candidate)
		return "", err
	}
	backup := path + ".orig"
	if _, err := os.Lstat(backup); err == nil {
		_ = os.Remove(candidate)
		return "", product.NewError(product.CodeStateConflict, "repair backup already exists")
	}
	if err := copySync(path, backup); err != nil {
		_ = os.Remove(candidate)
		_ = os.Remove(backup)
		return "", err
	}
	if err := store.ReplaceFile(candidate, path); err != nil {
		_ = os.Remove(backup)
		_ = os.Remove(candidate)
		return "", err
	}
	if err := store.SyncDir(dir); err != nil {
		return "", err
	}
	return backup, nil
}

func writeCandidate(path string, stored store.StoredSession, maxLine int) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := writeRecord(f, maxLine, stored.Header); err != nil {
		return err
	}
	for _, commit := range stored.Commits {
		if err := writeRecord(f, maxLine, commit); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := store.SyncDir(filepath.Dir(path)); err != nil {
		return err
	}
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	checked, err := ReadFile(in, stored.Header.SessionID, maxLine)
	if err != nil {
		return err
	}
	if checked.RepairRequired || checked.LastSeq != stored.LastSeq || len(checked.Commits) != len(stored.Commits) {
		return product.NewError(product.CodeStorageUnavailable, "repair candidate failed validation")
	}
	for i := range stored.Commits {
		if checked.Commits[i].CommitID != stored.Commits[i].CommitID || checked.Commits[i].CommitSeq != stored.Commits[i].CommitSeq {
			return product.NewError(product.CodeStorageUnavailable, "repair candidate failed validation")
		}
	}
	return nil
}

func writeRecord(f *os.File, maxLine int, v any) error {
	if maxLine == 0 {
		maxLine = config.DefaultLimits().MaxCommitLineBytes
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(raw)+1 > maxLine {
		return product.NewError(product.CodeInvalidArgument, "commit line exceeds limit")
	}
	_, err = f.Write(append(raw, '\n'))
	return err
}

func copySync(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func ReadFile(r io.Reader, sessionID string, maxLine int) (store.StoredSession, error) {
	if maxLine == 0 {
		maxLine = config.DefaultLimits().MaxCommitLineBytes
	}
	lines, incomplete, err := scanLines(r, maxLine)
	if err != nil {
		return store.StoredSession{}, err
	}
	if len(lines) == 0 {
		return store.StoredSession{}, product.NewError(product.CodeNotFound, "empty journal")
	}
	var header store.Header
	if err := json.Unmarshal(lines[0], &header); err != nil {
		return store.StoredSession{}, product.NewError(product.CodeIncompatibleVersion, "header is unreadable")
	}
	if header.RecordType != "header" || header.FormatVersion != formatVersion {
		return store.StoredSession{}, product.NewError(product.CodeIncompatibleVersion, "unknown journal version")
	}
	if sessionID != "" && header.SessionID != sessionID {
		return store.StoredSession{}, product.NewError(product.CodeNotFound, "session mismatch")
	}
	chain := store.NewChain()
	for i := 1; i < len(lines); i++ {
		var commit store.Commit
		if err := json.Unmarshal(lines[i], &commit); err != nil {
			return store.StoredSession{}, product.NewError(product.CodeIncompatibleVersion, "journal body is damaged")
		}
		if err := chain.Apply(commit); err != nil {
			return store.StoredSession{}, err
		}
	}
	return chain.Session(header, incomplete), nil
}

func scanLines(r io.Reader, maxLine int) ([][]byte, bool, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var lines [][]byte
	incomplete := false
	for {
		line, err := readLine(br, maxLine)
		if err == io.EOF && len(line) == 0 {
			break
		}
		if err != nil && err != io.EOF {
			return nil, false, err
		}
		if err == io.EOF {
			if len(bytes.TrimSpace(line)) > 0 {
				incomplete = true
			}
			break
		}
		body := bytes.TrimRight(line, "\n")
		if len(bytes.TrimSpace(body)) == 0 {
			return nil, false, product.NewError(product.CodeIncompatibleVersion, "journal body is damaged")
		}
		lines = append(lines, append([]byte(nil), body...))
	}
	return lines, incomplete, nil
}

func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		frag, err := r.ReadSlice('\n')
		buf = append(buf, frag...)
		if len(buf) > max+1 {
			return nil, product.NewError(product.CodeInvalidArgument, "journal line exceeds limit")
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return buf, err
		}
		return buf, nil
	}
}
