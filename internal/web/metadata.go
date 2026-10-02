package web

import (
	"context"
	"encoding/json"
	"os"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

// Metadata is display-only and never changes journal facts, targets or permissions.
type Metadata struct {
	Name     string   `json:"name"`
	Labels   []string `json:"labels"`
	Revision uint64   `json:"revision"`
}
type SetMetadataRequest struct {
	IdempotencyKey, Name string
	Labels               []string
}
type metadataKey struct {
	Key    string   `json:"key"`
	Digest string   `json:"digest"`
	Result Metadata `json:"result"`
}
type metadataFile struct {
	Metadata
	Keys []metadataKey `json:"keys"`
}

const metadataName = "metadata.json"

func (c *Catalog) SetMetadata(ctx context.Context, sid string, req SetMetadataRequest) (Metadata, bool, error) {
	if err := c.fileWriteAllowed(ctx, sid); err != nil {
		return Metadata{}, false, err
	}
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 256 {
		return Metadata{}, false, product.NewError(product.CodeInvalidArgument, "idempotency key is required")
	}
	if err := validDisplayText(req.Name, config.SessionNameBytes); err != nil {
		return Metadata{}, false, err
	}
	if len(req.Labels) > config.SessionLabels {
		return Metadata{}, false, product.NewError(product.CodeInvalidArgument, "too many labels")
	}
	labels, seen := make([]string, 0, len(req.Labels)), map[string]bool{}
	for _, l := range req.Labels {
		if l == "" || seen[l] || validDisplayText(l, config.SessionLabelBytes) != nil {
			return Metadata{}, false, product.NewError(product.CodeInvalidArgument, "label is invalid")
		}
		seen[l] = true
		labels = append(labels, l)
	}
	digest := digestOf(c.opts.Principal, sid, "set_metadata", req.Name, labels)
	c.files.Lock()
	defer c.files.Unlock()
	if err := c.beginFileWrite(ctx); err != nil {
		return Metadata{}, false, err
	}
	defer c.finishFileWrite()
	root, dir, err := c.sessionSubdir(sid, "", false)
	if err != nil {
		return Metadata{}, false, err
	}
	defer root.Close()
	file, err := loadMetadata(root)
	if err != nil {
		return Metadata{}, false, err
	}
	for _, k := range file.Keys {
		if k.Key == req.IdempotencyKey {
			if k.Digest != digest {
				return Metadata{}, false, product.NewError(product.CodeIdempotencyConflict, "key already belongs to a different metadata change")
			}
			return k.Result, true, nil
		}
	}
	file.Metadata = Metadata{Name: req.Name, Labels: labels, Revision: file.Revision + 1}
	file.Keys = append(file.Keys, metadataKey{Key: req.IdempotencyKey, Digest: digest, Result: file.Metadata})
	if extra := len(file.Keys) - config.SessionMetadataKeysKept; extra > 0 {
		file.Keys = file.Keys[extra:]
	}
	raw, _ := json.Marshal(file)
	if err = c.publish(root, dir, metadataName, raw); err != nil {
		return Metadata{}, false, err
	}
	return file.Metadata, false, nil
}
func (c *Catalog) GetMetadata(ctx context.Context, sid string) (Metadata, error) {
	if err := c.fileReadAllowed(ctx, sid); err != nil {
		return Metadata{}, err
	}
	root, _, err := c.sessionSubdir(sid, "", false)
	if err != nil {
		return Metadata{}, err
	}
	defer root.Close()
	file, err := loadMetadata(root)
	return file.Metadata, err
}
func loadMetadata(root *os.Root) (metadataFile, error) {
	file := metadataFile{Metadata: Metadata{Labels: []string{}}}
	raw, err := store.ReadRegular(root, metadataName, 1<<20)
	if e, ok := product.AsError(err); ok && e.Code == product.CodeNotFound {
		return file, nil
	}
	if err != nil {
		return metadataFile{}, err
	}
	if json.Unmarshal(raw, &file) != nil {
		return metadataFile{}, product.NewError(product.CodeStorageUnavailable, "session metadata is corrupt")
	}
	if file.Labels == nil {
		file.Labels = []string{}
	}
	return file, nil
}
