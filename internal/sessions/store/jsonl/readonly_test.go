package jsonl_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	product "github.com/ww1489/seasprak/internal/errors"
	contract "github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/jsonl"
)

func TestP2ReadOnlyPreservesFilesAndRejectsAppend(t *testing.T) {
	root := t.TempDir()
	writer, err := jsonl.Open("s1", root, contract.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "sessions", "s1")
	parents := []string{root, filepath.Dir(dir), dir}
	modes := make([]os.FileMode, len(parents))
	for i, parent := range parents {
		if err := os.Chmod(parent, 0755); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(parent)
		if err != nil {
			t.Fatal(err)
		}
		modes[i] = info.Mode()
	}
	if err = os.Remove(filepath.Join(dir, "writer.lock")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "journal.jsonl")
	if err = os.Chmod(path, 0444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0600) })
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := jsonl.Open("s1", root, contract.Header{}, jsonl.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = reader.Append(context.Background(), "s1", contract.ExpectedCommit{}, contract.Commit{CommitID: "forbidden"})
	var pe *product.Error
	if !errors.As(err, &pe) || pe.Code != product.CodePermissionDenied {
		t.Fatalf("append=%v", err)
	}
	if err = reader.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	afterEntries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, parent := range parents {
		info, err := os.Stat(parent)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != modes[i] {
			t.Fatal("read-only open changed directory permissions")
		}
	}
	if string(before) != string(after) || info.Mode() != afterInfo.Mode() || !info.ModTime().Equal(afterInfo.ModTime()) || !reflect.DeepEqual(entries, afterEntries) {
		t.Fatal("read-only open changed content, mode, time or directory")
	}
}

func TestP2ReadOnlyUsesBoundedPrefix(t *testing.T) {
	root := t.TempDir()
	writer, err := jsonl.Open("s1", root, contract.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := jsonl.Open("s1", root, contract.Header{}, jsonl.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err = writer.Append(context.Background(), "s1", contract.ExpectedCommit{}, contract.Commit{CommitID: "new"}); err != nil {
		t.Fatal(err)
	}
	old, err := reader.Load(context.Background(), "s1")
	if err != nil || old.LastSeq != 0 {
		t.Fatalf("snapshot changed: %d %v", old.LastSeq, err)
	}
	fresh, err := jsonl.Open("s1", root, contract.Header{}, jsonl.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	next, err := fresh.Load(context.Background(), "s1")
	if err != nil || next.LastSeq != 1 {
		t.Fatalf("new snapshot=%d %v", next.LastSeq, err)
	}
}

func TestP2ReadOnlyTornTailRemainsUntouched(t *testing.T) {
	root := t.TempDir()
	writer, err := jsonl.Open("s1", root, contract.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Append(context.Background(), "s1", contract.ExpectedCommit{}, contract.Commit{CommitID: "complete"}); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sessions", "s1", "journal.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(`{"commitId":"torn"`)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal(writeErr, closeErr)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := jsonl.Open("s1", root, contract.Header{}, jsonl.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	loaded, err := reader.Load(context.Background(), "s1")
	if err != nil || loaded.LastSeq != 1 || !loaded.RepairRequired {
		t.Fatalf("valid prefix=%+v error=%v", loaded, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("reader repaired torn journal")
	}
}

func TestP2ReadOnlyConcurrentPartialCommit(t *testing.T) {
	partial, finish := make(chan struct{}), make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(finish) }) }
	writes := 0
	root := t.TempDir()
	writer, err := jsonl.Open("s1", root, contract.Header{}, jsonl.Options{Write: func(f *os.File, p []byte) (int, error) {
		writes++
		if writes == 1 {
			return f.Write(p)
		}
		n, err := f.Write(p[:len(p)-1])
		close(partial)
		<-finish
		if err != nil {
			return n, err
		}
		last, err := f.Write(p[len(p)-1:])
		return n + last, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_, err := writer.Append(context.Background(), "s1", contract.ExpectedCommit{}, contract.Commit{CommitID: "concurrent"})
		done <- err
	}()
	t.Cleanup(func() {
		unblock()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("writer did not exit")
			return
		}
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	})
	select {
	case <-partial:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not reach partial commit")
	}
	reader, err := jsonl.Open("s1", root, contract.Header{}, jsonl.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	prefix, err := reader.Load(context.Background(), "s1")
	if err != nil || prefix.LastSeq != 0 || !prefix.RepairRequired {
		t.Fatalf("partial prefix=%+v error=%v", prefix, err)
	}
	unblock()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not complete commit")
	}
	// This reader remains fixed even after the writer completes the line.
	again, err := reader.Load(context.Background(), "s1")
	if err != nil || !reflect.DeepEqual(prefix, again) {
		t.Fatal("read-only snapshot changed", err)
	}
}

func TestP2ReadOnlyMissingNeverCreates(t *testing.T) {
	root := t.TempDir()
	_, err := jsonl.Open("missing", root, contract.Header{}, jsonl.Options{ReadOnly: true})
	if err == nil {
		t.Fatal("missing journal accepted")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("read-only open created files: %v", err)
	}
}
