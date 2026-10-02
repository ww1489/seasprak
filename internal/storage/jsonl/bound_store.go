package jsonl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

// OpenJournalReader opens only the checked ordinary journal in the borrowed
// root. The caller owns the returned file, never the root.
func OpenJournalReader(root *os.Root) (*os.File, error) {
	file, err := openJournalReaderWithFile(root, boundOpenFile)
	return file, normalizeBoundError(err)
}

func openJournalReaderWithFile(root *os.Root, open func(*os.Root, string, int, os.FileMode) (*os.File, error)) (*os.File, error) {
	return checkedBoundLeaf(root, "journal.jsonl", os.O_RDONLY, 0, open)
}

// OpenBound borrows roots and inspected while constructing the Store. Success
// transfers roots and the new journal/lock to Store; failure never closes roots.
// inspected is caller-owned on both success and failure and is never closed here.
func OpenBound(sessionID string, roots *store.ResourceRoots, inspected *os.File, header store.Header, opt Options) (*Store, error) {
	s, err := openBoundWithFile(sessionID, roots, inspected, header, opt, boundOpenFile)
	return s, normalizeBoundError(err)
}

func openBoundWithFile(sessionID string, roots *store.ResourceRoots, inspected *os.File, header store.Header, opt Options, open func(*os.Root, string, int, os.FileMode) (*os.File, error)) (out *Store, err error) {
	if err := store.ValidateResourceID(sessionID); err != nil {
		return nil, err
	}
	if roots == nil || roots.Namespace == nil || roots.Resource == nil || open == nil {
		return nil, product.NewError(product.CodeInvalidArgument, "resource roots and journal opener are required")
	}
	if opt.ResourceType != "" && opt.ResourceType != store.ResourceCode && opt.ResourceType != store.ResourceWorkflow {
		return nil, product.NewError(product.CodeInvalidArgument, "unsupported resource type")
	}
	if opt.ReadOnly {
		opt.OpenExisting = true
	}
	root := roots.Resource
	before, statErr := root.Lstat("journal.jsonl")
	missing := os.IsNotExist(statErr)
	if statErr != nil && !missing {
		return nil, statErr
	}
	if !missing && !ordinaryBoundFile(before) {
		return nil, product.NewError(product.CodeInvalidArgument, "journal must be an ordinary file")
	}
	if missing && opt.OpenExisting {
		return nil, product.NewError(product.CodeNotFound, "journal does not exist")
	}
	var ownedInspection, journal *os.File
	var lock *writerLock
	var s *Store
	defer func() {
		if ownedInspection != nil {
			err = errors.Join(err, ownedInspection.Close())
		}
		if err != nil {
			if s != nil {
				_ = s.Close()
				err = errors.Join(err, s.closeCause)
			} else {
				if journal != nil {
					err = errors.Join(err, journal.Close())
				}
				if lock != nil {
					err = errors.Join(err, lock.Close())
				}
			}
			out = nil
		}
	}()
	if inspected != nil && missing {
		return nil, product.NewError(product.CodeInvalidArgument, "inspected journal no longer has its binding")
	}
	if inspected == nil && !missing {
		ownedInspection, err = openJournalReaderWithFile(root, open)
		if err != nil {
			return nil, err
		}
		inspected = ownedInspection
	}
	maxLine := opt.MaxLine
	if maxLine == 0 {
		maxLine = config.DefaultLimits().MaxCommitLineBytes
	}
	empty := missing
	if inspected != nil {
		info, err := inspected.Stat()
		if err != nil {
			return nil, err
		}
		if !ordinaryBoundFile(info) || !os.SameFile(before, info) {
			return nil, product.NewError(product.CodeInvalidArgument, "inspected journal has invalid type or identity")
		}
		empty = info.Size() == 0
		if empty && opt.OpenExisting {
			return nil, product.NewError(product.CodeNotFound, "journal does not exist")
		}
		if !empty {
			// SectionReader leaves the caller's file offset unchanged. This exact
			// inspection file stays open until the final file identity is checked.
			line, readErr := readLine(bufio.NewReader(io.NewSectionReader(inspected, 0, info.Size())), maxLine)
			var existing store.Header
			if readErr != nil || json.Unmarshal(line, &existing) != nil {
				return nil, product.NewError(product.CodeIncompatibleVersion, "resource header is unreadable")
			}
			if err := store.ValidateHeader(existing, opt.ResourceType, sessionID); err != nil {
				return nil, err
			}
		}
	}
	if empty {
		header.RecordType = "header"
		header.FormatVersion = formatVersion
		header.ResourceType = opt.ResourceType
		if opt.ResourceType == store.ResourceWorkflow {
			header.RunID = sessionID
		} else {
			header.SessionID = sessionID
		}
		if err := store.ValidateHeader(header, opt.ResourceType, sessionID); err != nil {
			return nil, err
		}
		if header.CreatedAt.IsZero() {
			header.CreatedAt = time.Now().UTC()
		}
	}
	flags := os.O_RDONLY
	if !opt.ReadOnly {
		lock, err = openWriterLock(root)
		if err != nil {
			return nil, err
		}
		// Tighten only retained directories, after the writer lock succeeds.
		if err := roots.Namespace.Chmod(".", 0700); err != nil {
			return nil, err
		}
		if err := root.Chmod(".", 0700); err != nil {
			return nil, err
		}
		flags = os.O_RDWR
		if missing {
			flags |= os.O_CREATE | os.O_EXCL
		}
	}
	journal, err = checkedBoundLeaf(root, "journal.jsonl", flags, 0600, open)
	if err != nil {
		return nil, err
	}
	info, err := journal.Stat()
	if err != nil {
		return nil, err
	}
	if inspected != nil {
		inspectionInfo, err := inspected.Stat()
		if err != nil {
			return nil, err
		}
		if !ordinaryBoundFile(inspectionInfo) || !os.SameFile(info, inspectionInfo) {
			return nil, product.NewError(product.CodeInvalidArgument, "journal changed after inspection")
		}
	}
	if !opt.ReadOnly {
		if err := journal.Chmod(0600); err != nil {
			return nil, err
		}
	}
	if ownedInspection != nil {
		closeErr := ownedInspection.Close()
		ownedInspection = nil
		if closeErr != nil {
			return nil, closeErr
		}
	}
	s = &Store{
		dir: roots.Path, sessionID: sessionID, journal: journal, lock: lock,
		maxLine: maxLine, syncFile: opt.SyncFile, syncDir: opt.SyncDir, write: opt.Write,
		header: header, chain: store.NewChain(), readOnly: opt.ReadOnly, snapshotSize: info.Size(),
	}
	if s.syncFile == nil {
		s.syncFile = func(file *os.File) error { return file.Sync() }
	}
	// Preserve nil: journal and Blob defaults sync their retained roots;
	// only an explicit host option selects the path-based directory hook.
	if info.Size() == 0 {
		if opt.OpenExisting {
			return nil, product.NewError(product.CodeNotFound, "journal does not exist")
		}
		if err := s.writeLine(header); err != nil {
			return nil, err
		}
		if err := s.syncFile(journal); err != nil {
			return nil, err
		}
		if opt.SyncDir != nil {
			err = opt.SyncDir(roots.Path)
		} else {
			err = store.SyncRoot(root)
		}
		if err != nil {
			return nil, err
		}
	}
	if _, err := s.reload(); err != nil {
		return nil, err
	}
	// No constructor cleanup can close caller roots before this final transfer.
	s.roots = roots
	return s, nil
}

// ResourceRoot is borrowed by internal factories until Store.Close. It is not
// an authorization port; callers must finish all use before closing the Store.
func (s *Store) ResourceRoot() *os.Root {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.roots == nil {
		return nil
	}
	return s.roots.Resource
}

func ordinaryBoundFile(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && !store.IsReparseInfo(info)
}

func checkedBoundLeaf(root *os.Root, name string, flags int, mode os.FileMode, open func(*os.Root, string, int, os.FileMode) (*os.File, error)) (*os.File, error) {
	invalid := func() error {
		return product.NewError(product.CodeInvalidArgument, "journal or writer lock has invalid type or identity")
	}
	if root == nil || open == nil || name != "journal.jsonl" && name != "writer.lock" {
		return nil, invalid()
	}
	before, err := root.Lstat(name)
	create := flags&os.O_CREATE != 0
	if err != nil && !(create && os.IsNotExist(err)) {
		if os.IsNotExist(err) {
			return nil, product.NewError(product.CodeNotFound, "journal does not exist")
		}
		return nil, err
	}
	if err == nil && !ordinaryBoundFile(before) {
		return nil, invalid()
	}
	file, openErr := open(root, name, flags, mode)
	if openErr != nil {
		if file != nil {
			openErr = errors.Join(openErr, file.Close())
		}
		current, statErr := root.Lstat(name)
		if !create && (os.IsNotExist(statErr) || statErr == nil && (!ordinaryBoundFile(current) || !os.SameFile(before, current))) {
			return nil, errors.Join(invalid(), openErr)
		}
		return nil, openErr
	}
	fail := func(cause error) (*os.File, error) { return nil, errors.Join(cause, file.Close()) }
	opened, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	if !ordinaryBoundFile(opened) || !create && !os.SameFile(before, opened) {
		return fail(invalid())
	}
	current, err := root.Lstat(name)
	if err != nil {
		if os.IsNotExist(err) {
			return fail(invalid())
		}
		return fail(err)
	}
	if !ordinaryBoundFile(current) || !os.SameFile(opened, current) {
		return fail(invalid())
	}
	return file, nil
}

// Raw cleanup errors remain internal. Product errors stay directly assertable,
// context errors stay recognizable, and OS paths never enter public messages.
func normalizeBoundError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var pe *product.Error
	if errors.As(err, &pe) {
		return pe
	}
	return product.NewError(product.CodeStorageUnavailable, "session store open failed")
}
