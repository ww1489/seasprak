package memory

import (
	"context"
	"errors"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

func TestBlobCorruptMemoryAndClose(t *testing.T) {
	s, err := Open("blob", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	b, ok := any(s).(store.CheckpointBlobs)
	if !ok {
		t.Fatal("store does not implement checkpoint blobs")
	}
	ref, err := b.Put(context.Background(), "blob", []byte("checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	s.blobs[ref.Hash][0] ^= 1
	if data, e := b.Get(context.Background(), "blob", ref); data != nil || e == nil {
		t.Fatal("corrupt memory blob accepted")
	}
	if r, e := b.Put(context.Background(), "blob", []byte("checkpoint")); r != (store.BlobRef{}) || e == nil {
		t.Fatal("corrupt memory blob overwritten")
	}
	s.Close()
	r, err := b.Put(context.Background(), "blob", nil)
	var pe *product.Error
	if r != (store.BlobRef{}) || !errors.As(err, &pe) || pe.Code != product.CodeStateConflict {
		t.Fatalf("closed Put: %+v %v", r, err)
	}
}
