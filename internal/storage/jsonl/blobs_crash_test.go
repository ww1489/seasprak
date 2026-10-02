package jsonl_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	product "github.com/ww1489/seasprak/internal/errors"
	contract "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
)

// These are process-death tests, not power-loss durability or session execution
// recovery certification. Link has no injection hook: file-sync-after precedes
// the real Link, and blob-dir-sync-before follows Link and temporary-name removal.
func TestBlobCrashSubprocess(t *testing.T) {
	if window := os.Getenv("SEASPRAK_BLOB_CRASH_WINDOW"); window != "" {
		blobCrashChild(t, os.Getenv("SEASPRAK_BLOB_CRASH_ROOT"), window)
		return
	}
	for _, tc := range []struct {
		window    string
		published bool
		committed bool
		torn      bool
		conflict  bool
	}{
		{window: "parent-dir-sync-before"},
		{window: "parent-dir-sync-after"},
		{window: "file-sync-before"},
		{window: "file-sync-after"},
		{window: "blob-dir-sync-before", published: true},
		{window: "blob-dir-sync-after", published: true},
		{window: "put-returned", published: true},
		{window: "journal-write-before", published: true},
		{window: "journal-write-partial", published: true, torn: true},
		{window: "journal-write-after", published: true, committed: true},
		{window: "journal-sync-before", published: true, committed: true},
		{window: "journal-sync-after", published: true, committed: true},
		{window: "append-returned", published: true, committed: true},
		{window: "link-conflict-returned", conflict: true},
	} {
		t.Run(tc.window, func(t *testing.T) {
			root := t.TempDir()
			s := openStore(t, root)
			if _, err := s.Append(context.Background(), "s1", contract.ExpectedCommit{}, contract.Commit{CommitID: "prefix"}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			prefix, err := os.ReadFile(journalPath(root))
			if err != nil {
				t.Fatal(err)
			}
			blobCrashKill(t, root, tc.window)
			raw, err := os.ReadFile(journalPath(root))
			if err != nil || !bytes.HasPrefix(raw, prefix) {
				t.Fatalf("journal prefix changed: %v", err)
			}
			reopened, err := jsonl.Open("s1", root, contract.Header{}, jsonl.Options{OpenExisting: true})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			loaded, err := reopened.Load(context.Background(), "s1")
			if err != nil {
				t.Fatal(err)
			}
			wantSeq := uint64(1)
			if tc.committed {
				wantSeq = 2
			}
			if loaded.LastSeq != wantSeq || len(loaded.Commits) != int(wantSeq) || loaded.RepairRequired != tc.torn || loaded.Commits[0].CommitID != "prefix" {
				t.Fatalf("reopened journal: %+v", loaded)
			}
			data, ref := blobCrashData()
			path := filepath.Join(root, "sessions", "s1", "checkpoints", ref.Hash+".bin")
			got, getErr := reopened.Get(context.Background(), "s1", ref)
			switch {
			case tc.conflict:
				blobCrashError(t, getErr, product.CodeStorageUnavailable)
				before, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				returned, err := reopened.Put(context.Background(), "s1", data)
				blobCrashError(t, err, product.CodeStorageUnavailable)
				if returned != (contract.BlobRef{}) || got != nil {
					t.Fatal("conflicting blob returned usable reference or data")
				}
				after, err := os.Stat(path)
				if err != nil || !os.SameFile(before, after) {
					t.Fatalf("conflicting final name replaced: %v", err)
				}
				content, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(content, bytes.Repeat([]byte("x"), len(data))) {
					t.Fatalf("conflicting final content changed: %v", err)
				}
			case tc.published:
				if getErr != nil || !bytes.Equal(got, data) {
					t.Fatalf("published blob incomplete: data=%q err=%v", got, getErr)
				}
				before, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				// A torn journal intentionally disallows further writes.
				if !tc.torn {
					retry, err := reopened.Put(context.Background(), "s1", data)
					if err != nil || retry != ref {
						t.Fatalf("publication retry: ref=%+v err=%v", retry, err)
					}
					after, err := os.Stat(path)
					if err != nil || !os.SameFile(before, after) {
						t.Fatalf("retry replaced published file: %v", err)
					}
				}
			default:
				blobCrashError(t, getErr, product.CodeNotFound)
				if _, err := os.Stat(path); !os.IsNotExist(err) || got != nil {
					t.Fatalf("unpublished blob exposed: %v", err)
				}
			}
			refs := 0
			for _, commit := range loaded.Commits {
				for _, record := range commit.ControlRecords {
					var storedRef contract.BlobRef
					if err := json.Unmarshal(record.Payload, &storedRef); err != nil || storedRef != ref {
						t.Fatalf("journal reference: %+v err=%v", storedRef, err)
					}
					blob, err := reopened.Get(context.Background(), "s1", storedRef)
					if err != nil || !bytes.Equal(blob, data) {
						t.Fatalf("journal contains unavailable blob reference: %v", err)
					}
					refs++
				}
			}
			if refs != int(wantSeq-1) {
				t.Fatalf("journal reference count=%d, want %d", refs, wantSeq-1)
			}
			if tc.torn {
				_, err := reopened.Append(context.Background(), "s1", contract.ExpectedCommit{ExpectedPreviousSeq: 1}, contract.Commit{CommitID: "must-not-append", ExpectedPreviousSeq: 1})
				blobCrashError(t, err, product.CodeStorageUnavailable)
			}
			after, err := os.ReadFile(journalPath(root))
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatalf("reopen or verification changed journal: %v", err)
			}
		})
	}
}

func blobCrashData() ([]byte, contract.BlobRef) {
	data := []byte("synthetic checkpoint crash payload")
	return data, contract.BlobRef{Hash: fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data))}
}

func blobCrashError(t *testing.T, err error, code string) {
	t.Helper()
	var pe *product.Error
	if !errors.As(err, &pe) || pe.Code != code {
		t.Fatalf("error=%v, want code %s", err, code)
	}
}

func blobCrashChild(t *testing.T, root, window string) {
	stop := func(point string) {
		if point != window {
			return
		}
		fmtPrintln("BLOB-READY:" + point)
		// The parent keeps stdin open until Kill/Wait. No scheduler timing or
		// sleeping determines the crash window, and no deferred Close runs.
		var b [1]byte
		_, _ = io.ReadFull(os.Stdin, b[:])
		t.Fatal("crash barrier released without process termination")
	}
	data, want := blobCrashData()
	blobSyncs, blobDirSyncs, journalWrites, journalSyncs := 0, 0, 0, 0
	s, err := jsonl.Open("s1", root, contract.Header{}, jsonl.Options{
		OpenExisting: true,
		SyncFile: func(f *os.File) error {
			journal := filepath.Base(f.Name()) == "journal.jsonl"
			point := "file-sync"
			if journal {
				journalSyncs++
				point = "journal-sync"
			} else {
				blobSyncs++
			}
			stop(point + "-before")
			if err := f.Sync(); err != nil {
				return err
			}
			stop(point + "-after")
			if !journal && window == "link-conflict-returned" {
				// Put has already checked absence. Force its real non-replacing
				// Link through EEXIST with different content of the same size.
				path := filepath.Join(root, "sessions", "s1", "checkpoints", want.Hash+".bin")
				f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.Write(bytes.Repeat([]byte("x"), len(data))); err != nil {
					t.Fatal(err)
				}
				if err := f.Sync(); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		},
		SyncDir: func(path string) error {
			blobDir := filepath.Base(path) == "checkpoints"
			if blobDir {
				blobDirSyncs++
				stop("blob-dir-sync-before")
			} else {
				stop("parent-dir-sync-before")
			}
			if err := contract.SyncDir(path); err != nil {
				return err
			}
			if blobDir {
				stop("blob-dir-sync-after")
			} else {
				stop("parent-dir-sync-after")
			}
			return nil
		},
		Write: func(f *os.File, p []byte) (int, error) {
			if filepath.Base(f.Name()) != "journal.jsonl" {
				return f.Write(p)
			}
			journalWrites++
			stop("journal-write-before")
			if window == "journal-write-partial" {
				if _, err := f.Write(p[:len(p)/2]); err != nil {
					t.Fatal(err)
				}
				stop("journal-write-partial")
			}
			n, err := f.Write(p)
			if err == nil && n == len(p) {
				stop("journal-write-after")
			}
			return n, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.Put(context.Background(), "s1", data)
	if window == "link-conflict-returned" {
		blobCrashError(t, err, product.CodeStorageUnavailable)
		if ref != (contract.BlobRef{}) || blobSyncs != 1 || blobDirSyncs != 0 || journalWrites != 0 {
			t.Fatalf("failed publication: ref=%+v syncs=%d dirs=%d writes=%d", ref, blobSyncs, blobDirSyncs, journalWrites)
		}
		stop(window)
	}
	if err != nil || ref != want || blobSyncs != 1 || blobDirSyncs != 1 {
		t.Fatalf("Put: ref=%+v err=%v syncs=%d dirs=%d", ref, err, blobSyncs, blobDirSyncs)
	}
	stop("put-returned")
	payload, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	// This is a storage record carrying a blob reference, not a session checkpoint schema.
	receipt, err := s.Append(context.Background(), "s1", contract.ExpectedCommit{ExpectedPreviousSeq: 1}, contract.Commit{
		CommitID: "checkpoint", ExpectedPreviousSeq: 1, ControlRecords: []contract.Record{{Type: "checkpoint", Version: 1, ID: "synthetic", Payload: payload}},
	})
	if err != nil || receipt.CommitSeq != 2 || journalWrites != 1 || journalSyncs != 1 {
		t.Fatalf("Append: receipt=%+v err=%v writes=%d syncs=%d", receipt, err, journalWrites, journalSyncs)
	}
	stop("append-returned")
	t.Fatalf("window %q was not reached", window)
}

func blobCrashKill(t *testing.T, root, window string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestBlobCrashSubprocess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "SEASPRAK_BLOB_CRASH_ROOT="+root, "SEASPRAK_BLOB_CRASH_WINDOW="+window)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	ready := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(out)
		var lines []string
		for scanner.Scan() {
			line := scanner.Text()
			if line == "BLOB-READY:"+window {
				ready <- nil
				return
			}
			lines = append(lines, line)
		}
		ready <- fmt.Errorf("barrier not reached: %s (scan=%v)", strings.Join(lines, "\n"), scanner.Err())
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("child did not reach crash barrier")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || cmd.ProcessState.Success() {
		t.Fatalf("child not killed: wait=%v stderr=%s", waitErr, stderr.String())
	}
	t.Logf("killed at %s; Wait=%v", window, waitErr)
}
