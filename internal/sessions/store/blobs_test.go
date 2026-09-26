package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/jsonl"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
)

func TestBlobContract(t *testing.T) {
	for _, backend := range []string{"memory", "jsonl"} {
		t.Run(backend, func(t *testing.T) {
			var s store.Store
			var err error
			if backend == "memory" {
				s, err = memory.Open("blob-session", store.Header{})
			} else {
				s, err = jsonl.Open("blob-session", t.TempDir(), store.Header{}, jsonl.Options{})
			}
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			blobs, ok := s.(store.CheckpointBlobs)
			if !ok {
				t.Fatal("store does not implement checkpoint blobs")
			}
			ctx := context.Background()
			input := []byte("immutable checkpoint")
			original := bytes.Clone(input)
			ref, err := blobs.Put(ctx, "blob-session", input)
			if err != nil {
				t.Fatal(err)
			}
			if ref.Hash != fmt.Sprintf("%x", sha256.Sum256(original)) || ref.Size != int64(len(original)) {
				t.Fatalf("invalid ref: %+v", ref)
			}
			input[0] ^= 1
			got, err := blobs.Get(ctx, "blob-session", ref)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("input alias or Get failure: %v", err)
			}
			got[0] ^= 1
			again, err := blobs.Get(ctx, "blob-session", ref)
			if err != nil || !bytes.Equal(again, original) {
				t.Fatalf("output alias or Get failure: %v", err)
			}
			repeat, err := blobs.Put(ctx, "blob-session", original)
			if err != nil || repeat != ref {
				t.Fatalf("repeat changed ref: %+v %v", repeat, err)
			}
			for _, id := range []string{"other", "../blob-session", "", `..\blob-session`} {
				if bad, err := blobs.Put(ctx, id, original); err == nil || bad != (store.BlobRef{}) {
					t.Fatalf("cross-session Put accepted: %q", id)
				}
				if data, err := blobs.Get(ctx, id, ref); err == nil || data != nil {
					t.Fatalf("cross-session Get accepted: %q", id)
				}
			}
			for _, bad := range []store.BlobRef{{Hash: "../bad", Size: 1}, {Hash: strings.Repeat("a", 63)}, {Hash: strings.Repeat("G", 64)}, {Hash: strings.ToUpper(ref.Hash), Size: ref.Size}, {Hash: ref.Hash, Size: -1}, {Hash: ref.Hash, Size: ref.Size + 1}} {
				if data, err := blobs.Get(ctx, "blob-session", bad); err == nil || data != nil {
					t.Fatalf("invalid ref accepted: %+v", bad)
				}
			}
			empty, err := blobs.Put(ctx, "blob-session", nil)
			if err != nil || empty.Size != 0 {
				t.Fatalf("empty Put: %v", err)
			}
			if data, err := blobs.Get(ctx, "blob-session", empty); err != nil || len(data) != 0 {
				t.Fatalf("empty Get: %v", err)
			}
			var wg sync.WaitGroup
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for n := 0; n < 10; n++ {
						r, e := blobs.Put(ctx, "blob-session", original)
						if e != nil || r != ref {
							t.Errorf("concurrent Put: %v", e)
							return
						}
						b, e := blobs.Get(ctx, "blob-session", ref)
						if e != nil || !bytes.Equal(b, original) {
							t.Errorf("concurrent Get: %v", e)
							return
						}
					}
				}()
			}
			wg.Wait()
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if r, e := blobs.Put(cancelled, "blob-session", original); e != context.Canceled || r != (store.BlobRef{}) {
				t.Fatalf("cancelled Put: %v", e)
			}
			if b, e := blobs.Get(cancelled, "blob-session", ref); e != context.Canceled || b != nil {
				t.Fatalf("cancelled Get: %v", e)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if _, e := blobs.Get(ctx, "blob-session", ref); !blobCode(e, product.CodeStateConflict) {
				t.Fatalf("closed Get: %v", e)
			}
		})
	}
}

func blobCode(err error, code string) bool {
	e, ok := err.(*product.Error)
	return ok && e.Code == code
}
