package jsonl_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/ww1489/seasprak/agent"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/model"
	"github.com/ww1489/seasprak/session/history"
)

func TestSyncFailureKeepsUncertainBytes(t *testing.T) {
	root := t.TempDir()
	calls := 0
	store, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{SyncFile: func(f *os.File) error {
		calls++
		if calls > 1 {
			return os.ErrClosed
		}
		return f.Sync()
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append(context.Background(), "s1", history.ExpectedCommit{IdempotencyKey: "k", ContentDigest: []byte("d")}, history.Commit{CommitID: "c1"})
	pe, ok := err.(*model.Error)
	if !ok || pe.Code != model.CodeStorageUnavailable {
		t.Fatalf("err = %v", err)
	}
	path := journalPath(root)
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Contains(raw, []byte(`"commitId":"c1"`)) {
		t.Fatalf("uncertain commit was truncated away: %s", raw)
	}
	if _, err := store.Append(context.Background(), "s1", history.ExpectedCommit{ExpectedPreviousSeq: 1}, history.Commit{CommitID: "c2"}); err == nil {
		t.Fatal("writes must stop after an uncertain commit")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	dup, err := again.Append(context.Background(), "s1", history.ExpectedCommit{IdempotencyKey: "k", ContentDigest: []byte("d")}, history.Commit{CommitID: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if !dup.Duplicate || dup.CommitSeq != 1 {
		t.Fatalf("replayed receipt = %+v", dup)
	}
}

func TestShortWriteDoesNotTruncate(t *testing.T) {
	root := t.TempDir()
	writes := 0
	store, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{Write: func(f *os.File, p []byte) (int, error) {
		writes++
		if writes == 1 {
			return f.Write(p)
		}
		n, err := f.Write(p[:3])
		if err != nil {
			return n, err
		}
		return n, io.ErrShortWrite
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	before, err := os.ReadFile(journalPath(root))
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append(context.Background(), "s1", history.ExpectedCommit{}, history.Commit{CommitID: "c1"})
	pe, ok := err.(*model.Error)
	if !ok || pe.Code != model.CodeStorageUnavailable {
		t.Fatalf("err = %v", err)
	}
	after, err := os.ReadFile(journalPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) < len(before)+3 {
		t.Fatalf("short write was truncated: before %d after %d", len(before), len(after))
	}
	loaded, err := store.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LastSeq != 0 || !loaded.RepairRequired {
		t.Fatalf("short write acknowledged: %+v", loaded)
	}
}

func TestLineLargerThanScannerDefault(t *testing.T) {
	root := t.TempDir()
	store, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	payload := append(append([]byte(`{"text":"`), bytes.Repeat([]byte("a"), 70*1024)...), '"', '}')
	_, err = store.Append(context.Background(), "s1", history.ExpectedCommit{}, history.Commit{
		CommitID: "c1",
		Entries:  []history.Record{{Type: "message", Version: 1, ID: "m1", Payload: payload}},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LastSeq != 1 || len(loaded.Commits[0].Entries[0].Payload) != len(payload) {
		t.Fatalf("large line was not kept: seq %d", loaded.LastSeq)
	}
}

func TestValidJSONWithoutNewlineIsNotCommitted(t *testing.T) {
	root := t.TempDir()
	store := openStore(t, root)
	if _, err := store.Append(context.Background(), "s1", history.ExpectedCommit{}, history.Commit{CommitID: "c1"}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	path := journalPath(root)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(history.Commit{RecordType: "commit", Version: 1, CommitID: "c2", CommitSeq: 2, ExpectedPreviousSeq: 1})
	if _, err := f.Write(body); err != nil {
		t.Fatal(err)
	}
	f.Close()
	reopen, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	loaded, err := reopen.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.RepairRequired || loaded.LastSeq != 1 {
		t.Fatalf("tail without LF adopted: %+v", loaded)
	}
	if _, err := reopen.Append(context.Background(), "s1", history.ExpectedCommit{ExpectedPreviousSeq: 1}, history.Commit{CommitID: "c3"}); err == nil {
		t.Fatal("repair-required journal accepted a write")
	}
}

func TestMiddleDamageIsRejected(t *testing.T) {
	root := t.TempDir()
	path := writeJournal(t, root, []string{
		headerLine(),
		commitLine("c1", 1, 0),
		`{"recordType":"commit","broken":`,
		commitLine("c2", 2, 1),
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err == nil {
		t.Fatal("middle damage was accepted")
	}
	_, err = jsonl.Repair(path, 0)
	if err == nil {
		t.Fatal("repair skipped middle damage")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed repair changed the original journal")
	}
}

func TestFutureVersionAndChainValidation(t *testing.T) {
	root := t.TempDir()
	writeJournal(t, root, []string{
		headerLine(),
		`{"recordType":"commit","version":2,"commitId":"c1","commitSeq":1,"expectedPreviousSeq":0}`,
	})
	_, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	assertIncompatible(t, err)

	root = t.TempDir()
	writeJournal(t, root, []string{
		headerLine(),
		`{"recordType":"commit","version":1,"commitId":"c1","commitSeq":1,"expectedPreviousSeq":4}`,
	})
	_, err = jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err == nil {
		t.Fatal("expectedPreviousSeq was not checked")
	}

	root = t.TempDir()
	writeJournal(t, root, []string{
		headerLine(),
		commitLine("same", 1, 0),
		commitLine("same", 2, 1),
	})
	_, err = jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err == nil {
		t.Fatal("duplicate commit id was accepted")
	}

	root = t.TempDir()
	writeJournal(t, root, []string{
		headerLine(),
		commitWithEntry("c1", 1, 0, "m1", ""),
		commitWithEntry("c2", 2, 1, "m2", "missing"),
	})
	_, err = jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err == nil {
		t.Fatal("dangling parent was accepted")
	}

	root = t.TempDir()
	writeJournal(t, root, []string{
		headerLine(),
		commitWithControlParent("c1", 1, 0, "missing"),
	})
	store, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	root = t.TempDir()
	seq := uint64(5)
	body, _ := json.Marshal(history.Commit{
		RecordType: "commit", Version: 1, CommitID: "c1", CommitSeq: 1,
		Events: []agent.Event{{
			SchemaVersion: 1, Type: "message.finalized", Scope: agent.EventScope{SessionID: "s1"},
			EventID: "e1", DurableSeq: &seq, Payload: []byte(`{}`),
		}},
	})
	writeJournal(t, root, []string{headerLine(), string(body)})
	_, err = jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err == nil {
		t.Fatal("durable sequence gap was accepted")
	}
}

func TestReadAfterAndCopy(t *testing.T) {
	root := t.TempDir()
	store := openStore(t, root)
	if _, err := store.Append(context.Background(), "s1", history.ExpectedCommit{}, history.Commit{
		CommitID: "c1",
		Entries:  []history.Record{{Type: "message", Version: 1, ID: "m1", Payload: []byte(`{"n":1}`)}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(context.Background(), "s1", history.ExpectedCommit{ExpectedPreviousSeq: 1}, history.Commit{CommitID: "c2", ExpectedPreviousSeq: 1}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Commits[0].Entries[0].Payload[2] = 'X'
	again, err := store.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(again.Commits[0].Entries[0].Payload, []byte("X")) {
		t.Fatalf("load aliased payload: %s", again.Commits[0].Entries[0].Payload)
	}
	reader, err := store.ReadAfter(context.Background(), "s1", 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if got.CommitID != "c2" {
		t.Fatalf("ReadAfter = %s", got.CommitID)
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("end = %v", err)
	}
}

func TestRepairHoldsWriterLockAndKeepsOriginalOnFailure(t *testing.T) {
	root := t.TempDir()
	store := openStore(t, root)
	if _, err := store.Append(context.Background(), "s1", history.ExpectedCommit{}, history.Commit{CommitID: "c1"}); err != nil {
		t.Fatal(err)
	}
	path := journalPath(root)
	store.Close()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"recordType":"commit"`)
	f.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lk := flock.New(filepath.Join(filepath.Dir(path), "writer.lock"))
	ok, err := lk.TryLock()
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := jsonl.Repair(path, 0); err == nil {
		_ = lk.Unlock()
		t.Fatal("repair ran while the writer lock was held")
	}
	if err := lk.Unlock(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected repair changed the journal")
	}
	store.Close()
	backup, err := jsonl.Repair(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if backup != path+".orig" {
		t.Fatalf("backup = %s", backup)
	}
	orig, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(orig, before) {
		t.Fatal("backup is not the original journal")
	}
	reopen, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	loaded, err := reopen.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RepairRequired || loaded.LastSeq != 1 {
		t.Fatalf("repaired journal = %+v", loaded)
	}
}

func TestCrossProcessWriterExcluded(t *testing.T) {
	if os.Getenv("SEASPRAK_LOCK_CHILD") == "1" {
		_, err := jsonl.Open("s1", os.Getenv("SEASPRAK_LOCK_ROOT"), history.Header{}, jsonl.Options{})
		if err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	root := t.TempDir()
	store := openStore(t, root)
	defer store.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrossProcessWriterExcluded$", "-test.count=1")
	cmd.Env = append(os.Environ(), "SEASPRAK_LOCK_CHILD=1", "SEASPRAK_LOCK_ROOT="+root)
	err := cmd.Run()
	if err == nil {
		t.Fatal("second process acquired the writer lock")
	}
}

func TestCommitCrashSubprocess(t *testing.T) {
	if os.Getenv("SEASPRAK_CRASH_CHILD") == "1" {
		root := os.Getenv("SEASPRAK_CRASH_ROOT")
		store, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
		if err != nil {
			os.Exit(2)
		}
		if _, err := store.Append(context.Background(), "s1", history.ExpectedCommit{}, history.Commit{CommitID: "c1"}); err != nil {
			os.Exit(3)
		}
		f, err := os.OpenFile(filepath.Join(root, "sessions", "s1", "journal.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(4)
		}
		if _, err := f.WriteString(`{"commitId":"torn"`); err != nil {
			os.Exit(5)
		}
		if err := f.Sync(); err != nil {
			os.Exit(6)
		}
		fmtPrintln("READY")
		time.Sleep(30 * time.Second)
		_ = f.Close()
		_ = store.Close()
		os.Exit(0)
	}
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCommitCrashSubprocess$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "SEASPRAK_CRASH_CHILD=1", "SEASPRAK_CRASH_ROOT="+root)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if sc.Text() == "READY" {
				close(ready)
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("crash child did not become ready")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	t.Logf("child killed, wait=%v", waitErr)
	reopen, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	loaded, err := reopen.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.RepairRequired || loaded.LastSeq != 1 || len(loaded.Commits) != 1 || loaded.Commits[0].CommitID != "c1" {
		t.Fatalf("after kill %v journal = %+v", waitErr, loaded)
	}
	raw, err := os.ReadFile(filepath.Join(root, "sessions", "s1", "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"commitId":"torn"`)) {
		t.Fatalf("killed child lost the torn tail: %s", raw)
	}
}

func fmtPrintln(s string) {
	_, _ = os.Stdout.WriteString(s + "\n")
}

func TestCommitCrashKeepsPrefix(t *testing.T) {
	root := t.TempDir()
	store := openStore(t, root)
	if _, err := store.Append(context.Background(), "s1", history.ExpectedCommit{}, history.Commit{CommitID: "c1"}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	f, err := os.OpenFile(journalPath(root), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"commitId":"torn"`)
	f.Close()
	reopen, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	loaded, err := reopen.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.RepairRequired || loaded.LastSeq != 1 {
		t.Fatalf("crashed tail = %+v", loaded)
	}
	_, err = reopen.Append(context.Background(), "s1", history.ExpectedCommit{ExpectedPreviousSeq: 1}, history.Commit{CommitID: "c2"})
	if err == nil {
		t.Fatal("crashed journal accepted a write")
	}
}

func TestOpenExistingDoesNotCreate(t *testing.T) {
	root := t.TempDir()
	_, err := jsonl.Open("missing", root, history.Header{}, jsonl.Options{OpenExisting: true})
	pe, ok := err.(*model.Error)
	if !ok || pe.Code != model.CodeNotFound {
		t.Fatalf("missing open = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "sessions")); !os.IsNotExist(statErr) {
		t.Fatalf("open existing created sessions: %v", statErr)
	}
	created := openStore(t, root)
	created.Close()
	again, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{OpenExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}

func TestLoadChecksSessionAndContext(t *testing.T) {
	root := t.TempDir()
	store := openStore(t, root)
	defer store.Close()
	_, err := store.Load(context.Background(), "other")
	pe, ok := err.(*model.Error)
	if !ok || pe.Code != model.CodeNotFound {
		t.Fatalf("load other = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.Append(ctx, "s1", history.ExpectedCommit{}, history.Commit{CommitID: "c1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("append canceled = %v", err)
	}
}

func TestClosedJournalIsIdle(t *testing.T) {
	root := t.TempDir()
	store := openStore(t, root)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := store.Append(context.Background(), "s1", history.ExpectedCommit{}, history.Commit{CommitID: "c1"})
	pe, ok := err.(*model.Error)
	if !ok || pe.Code != model.CodeStateConflict {
		t.Fatalf("append after close = %v", err)
	}
}

func TestSessionIDAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"..", "../x", `a\b`, "a/b", `C:s`, "/abs"} {
		if _, err := jsonl.Open(id, root, history.Header{}, jsonl.Options{}); err == nil {
			t.Fatalf("id %q accepted", id)
		}
	}
	outside := t.TempDir()
	link := filepath.Join(root, "sessions")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			cmd := exec.Command("cmd", "/c", "mklink", "/J", link, outside)
			if out, jerr := cmd.CombinedOutput(); jerr != nil {
				t.Skipf("cannot create symlink or junction: %v %s", err, out)
			}
		} else {
			t.Fatal(err)
		}
	}
	_, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err == nil {
		t.Fatal("session directory escaped through a symlink or junction")
	}
}

func TestUnixPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ACLs do not use Unix mode bits")
	}
	root := t.TempDir()
	store := openStore(t, root)
	store.Close()
	dir := filepath.Join(root, "sessions", "s1")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %o", info.Mode().Perm())
	}
	info, err = os.Stat(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o", info.Mode().Perm())
	}
}

func openStore(t *testing.T, root string) *jsonl.Store {
	t.Helper()
	store, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func journalPath(root string) string {
	return filepath.Join(root, "sessions", "s1", "journal.jsonl")
}

func headerLine() string {
	raw, _ := json.Marshal(history.Header{RecordType: "header", FormatVersion: 1, SessionID: "s1"})
	return string(raw)
}

func commitLine(id string, seq, prev uint64) string {
	raw, _ := json.Marshal(history.Commit{
		RecordType: "commit", Version: 1, CommitID: id, CommitSeq: seq, ExpectedPreviousSeq: prev,
	})
	return string(raw)
}

func commitWithEntry(id string, seq, prev uint64, entryID, parent string) string {
	raw, _ := json.Marshal(history.Commit{
		RecordType: "commit", Version: 1, CommitID: id, CommitSeq: seq, ExpectedPreviousSeq: prev,
		Entries: []history.Record{{Type: "message", Version: 1, ID: entryID, ParentID: parent, Payload: []byte(`{}`)}},
	})
	return string(raw)
}

func commitWithControlParent(id string, seq, prev uint64, parent string) string {
	raw, _ := json.Marshal(history.Commit{
		RecordType: "commit", Version: 1, CommitID: id, CommitSeq: seq, ExpectedPreviousSeq: prev,
		ControlRecords: []history.Record{{Type: "budget", Version: 1, ID: "s1", ParentID: parent, Payload: []byte(`{}`)}},
	})
	return string(raw)
}

func writeJournal(t *testing.T, root string, lines []string) string {
	t.Helper()
	dir := filepath.Join(root, "sessions", "s1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "journal.jsonl")
	var buf bytes.Buffer
	for _, line := range lines {
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertIncompatible(t *testing.T, err error) {
	t.Helper()
	pe, ok := err.(*model.Error)
	if !ok || pe.Code != model.CodeIncompatibleVersion {
		t.Fatalf("err = %v, want incompatible_version", err)
	}
}
