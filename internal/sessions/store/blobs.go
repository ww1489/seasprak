package store

import (
	"context"
	"crypto/sha256"
	"fmt"

	product "github.com/ww1489/seasprak/internal/errors"
)

// BlobRef identifies immutable checkpoint bytes within a session. It is not
// permission to resume execution; that requires a committed product association.
type BlobRef struct {
	Hash string
	Size int64
}

// CheckpointBlobs is an optional capability independent of Store. Implementations
// bind every operation to their session. There is intentionally no Delete method.
// Memory implementations do not promise durability across process restarts.
type CheckpointBlobs interface {
	Put(context.Context, string, []byte) (BlobRef, error)
	Get(context.Context, string, BlobRef) ([]byte, error)
}

// ValidateBlobRef requires a canonical lowercase SHA-256 digest and byte count.
func ValidateBlobRef(ref BlobRef) error {
	if len(ref.Hash) != sha256.Size*2 || ref.Size < 0 {
		return product.NewError(product.CodeInvalidArgument, "invalid checkpoint reference")
	}
	for _, c := range ref.Hash {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return product.NewError(product.CodeInvalidArgument, "invalid checkpoint hash")
		}
	}
	return nil
}

// VerifyBlob checks the complete bytes, including their declared length.
func VerifyBlob(ref BlobRef, data []byte) error {
	if int64(len(data)) != ref.Size || fmt.Sprintf("%x", sha256.Sum256(data)) != ref.Hash {
		return product.NewError(product.CodeStorageUnavailable, "checkpoint integrity check failed")
	}
	return nil
}
