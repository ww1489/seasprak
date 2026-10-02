package jsonl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

func TestBlobLinkExistingTargetRace(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(map[bool]string{true: "same-content", false: "different-content"}[same], func(t *testing.T) {
			s, blobs, _ := blobStore(t)
			data := []byte("checkpoint")
			want := store.BlobRef{Hash: fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data))}
			path := filepath.Join(s.dir, "checkpoints", want.Hash+".bin")
			existing := bytes.Clone(data)
			if !same {
				existing[0] ^= 1 // Same length: rejection must check content, not only size.
			}
			var original os.FileInfo
			syncs := 0
			s.syncFile = func(f *os.File) error {
				syncs++
				if err := f.Sync(); err != nil {
					t.Fatal(err)
				}
				// Put already checked that the final name was absent. Publish a
				// competing file now so the real Link must take its EEXIST branch.
				competitor, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := competitor.Write(existing); err != nil {
					competitor.Close()
					t.Fatal(err)
				}
				if err := competitor.Close(); err != nil {
					t.Fatal(err)
				}
				original, err = os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				return nil
			}
			ref, err := blobs.Put(context.Background(), "blob", data)
			if same {
				if err != nil || ref != want {
					t.Fatalf("same-content race: ref=%+v err=%v", ref, err)
				}
				got, err := blobs.Get(context.Background(), "blob", ref)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("reused blob: data=%q err=%v", got, err)
				}
			} else {
				requireBlobError(t, err, product.CodeStorageUnavailable)
				if ref != (store.BlobRef{}) {
					t.Fatalf("conflicting publication returned ref: %+v", ref)
				}
			}
			if syncs != 1 {
				t.Fatalf("file sync calls=%d, want 1", syncs)
			}
			after, err := os.Stat(path)
			if err != nil || !os.SameFile(original, after) {
				t.Fatalf("competing file was replaced: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, existing) {
				t.Fatalf("competing content was overwritten: %v", err)
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil || len(entries) != 1 || entries[0].Name() != want.Hash+".bin" {
				t.Fatalf("temporary or fallback file retained: entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestBlobLinkFailureHasNoFallback(t *testing.T) {
	s, blobs, _ := blobStore(t)
	data := []byte("checkpoint")
	var held, temporary string
	var source, directory os.FileInfo
	syncs := 0
	s.syncFile = func(f *os.File) error {
		syncs++
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		// A directory cannot be a hard-link source. Preserve the already
		// synced file under another name and replace only its temporary name.
		tmp := filepath.Join(s.dir, "checkpoints", filepath.Base(f.Name()))
		temporary = tmp
		var err error
		source, err = f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		held = tmp + ".held"
		if err := os.Rename(tmp, held); err != nil {
			t.Fatal("prepare directory hard-link source:", err)
		}
		if err := os.Mkdir(tmp, 0700); err != nil {
			t.Fatal(err)
		}
		directory, err = os.Lstat(tmp)
		if err != nil || !directory.IsDir() {
			t.Fatal("fixture did not create foreign directory:", err)
		}
		return nil
	}
	ref, err := blobs.Put(context.Background(), "blob", data)
	requireBlobError(t, err, product.CodeStorageUnavailable)
	if ref != (store.BlobRef{}) || syncs != 1 {
		t.Fatalf("failed publication: ref=%+v syncs=%d", ref, syncs)
	}
	got, err := os.ReadFile(held)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("synced source changed: %v", err)
	}
	current, err := os.Lstat(held)
	if err != nil || !os.SameFile(source, current) {
		t.Fatalf("synced source identity changed: %v", err)
	}
	current, err = os.Lstat(temporary)
	if err != nil || !current.IsDir() || !os.SameFile(directory, current) {
		t.Errorf("foreign directory identity was deleted or changed: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, "checkpoints"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("source/directory removed or fallback file created: entries=%v err=%v", entries, err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(held) && entry.Name() != filepath.Base(temporary) {
			t.Fatalf("final or fallback file created: %s", entry.Name())
		}
	}
}

func TestBlobRejectReplacedJournalBinding(t *testing.T) {
	s, blobs, _ := blobStore(t)
	ctx := context.Background()
	data := []byte("checkpoint")
	ref, err := blobs.Put(ctx, "blob", data)
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(s.dir, "journal.jsonl")
	original, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := s.journal.Stat()
	if err != nil {
		t.Fatal(err)
	}
	// Windows protects the normal journal handle from rename. Release it
	// for fixture setup, then retain a handle to the same original file under
	// its new name. The production binding check still compares real identities.
	if err := s.journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(journal, journal+".original"); err != nil {
		t.Fatal(err)
	}
	s.journal, err = os.OpenFile(journal+".original", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := s.journal.Stat()
	if err != nil || !os.SameFile(bound, retained) {
		t.Fatalf("fixture lost the original journal identity: %v", err)
	}
	if err := os.WriteFile(journal, original, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(filepath.Join(s.dir, "checkpoints"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := blobs.Get(ctx, "blob", ref)
	requireBlobError(t, err, product.CodeStorageUnavailable)
	if got != nil {
		t.Fatal("changed binding returned blob bytes")
	}
	for _, input := range [][]byte{data, []byte("new checkpoint")} {
		ref, err := blobs.Put(ctx, "blob", input)
		requireBlobError(t, err, product.CodeStorageUnavailable)
		if ref != (store.BlobRef{}) {
			t.Fatalf("changed binding returned ref: %+v", ref)
		}
	}
	after, err := os.ReadDir(filepath.Join(s.dir, "checkpoints"))
	if err != nil || len(before) != 1 || len(after) != 1 || before[0].Name() != after[0].Name() {
		t.Fatalf("changed binding wrote checkpoint files: entries=%v err=%v", after, err)
	}
	got, err = os.ReadFile(filepath.Join(s.dir, "checkpoints", ref.Hash+".bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("changed binding altered original blob: %v", err)
	}
	got, err = os.ReadFile(journal)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("changed binding altered replacement journal: %v", err)
	}
}
