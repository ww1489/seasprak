package eino

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"fmt"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
)

type checkpointBlobMemory struct {
	data   map[string][]byte
	writes int
}

func (b *checkpointBlobMemory) PutCheckpoint(_ context.Context, data []byte) (agent.CheckpointBlobRef, error) {
	if b.data == nil {
		b.data = make(map[string][]byte)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	b.data[hash] = append([]byte(nil), data...)
	b.writes++
	return agent.CheckpointBlobRef{Hash: hash, Size: int64(len(data))}, nil
}
func (b *checkpointBlobMemory) GetCheckpoint(_ context.Context, ref agent.CheckpointBlobRef) ([]byte, error) {
	return append([]byte(nil), b.data[ref.Hash]...), nil
}
func TestCheckpointStoreDeletePreservesReferencedBlob(t *testing.T) {
	blobs := &checkpointBlobMemory{}
	s := NewCheckpointStore(blobs)
	if err := s.Set(t.Context(), "exec", []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	ref, ok := s.Ref("exec")
	if !ok || ref.Hash == "" || blobs.writes != 1 {
		t.Fatalf("ref=%+v exists=%v writes=%d", ref, ok, blobs.writes)
	}
	if _, ok, err := s.Get(t.Context(), "different"); err != nil || ok {
		t.Fatalf("other key: %v %v", ok, err)
	}
	if err := s.Delete(t.Context(), "exec"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(t.Context(), "exec"); ok {
		t.Fatal("deleted alias still readable")
	}
	if got, err := blobs.GetCheckpoint(t.Context(), ref); err != nil || string(got) != "checkpoint" {
		t.Fatalf("referenced blob was deleted: %q %v", got, err)
	}
}
func TestCheckpointWithoutRunnerStateCannotPause(t *testing.T) {
	var out bytes.Buffer
	if err := gob.NewEncoder(&out).Encode(struct {
		RunnerCheckpoint []byte
		HasRunnerState   bool
		UnhandledItems   []agent.InputRef
		CanceledItems    []agent.InputRef
	}{CanceledItems: []agent.InputRef{checkpointPrompt}}); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePausedCheckpoint(out.Bytes(), checkpointPrompt); err == nil {
		t.Fatal("checkpoint without runner state was accepted")
	}
}
