package agent

import "context"

// CheckpointBlobPort is the execution-layer boundary for immutable session blobs.
type CheckpointBlobPort interface {
	PutCheckpoint(context.Context, []byte) (CheckpointBlobRef, error)
	GetCheckpoint(context.Context, CheckpointBlobRef) ([]byte, error)
}

type CheckpointBlobRef struct {
	Hash string
	Size int64
}
