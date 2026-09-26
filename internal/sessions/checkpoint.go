package sessions

import (
	"context"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

type checkpointBlobs struct {
	store   store.CheckpointBlobs
	session string
}

func (b checkpointBlobs) PutCheckpoint(ctx context.Context, data []byte) (agent.CheckpointBlobRef, error) {
	ref, err := b.store.Put(ctx, b.session, data)
	return agent.CheckpointBlobRef{Hash: ref.Hash, Size: ref.Size}, err
}
func (b checkpointBlobs) GetCheckpoint(ctx context.Context, ref agent.CheckpointBlobRef) ([]byte, error) {
	return b.store.Get(ctx, b.session, store.BlobRef{Hash: ref.Hash, Size: ref.Size})
}
