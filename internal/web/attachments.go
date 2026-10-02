package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/codeagent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

type SaveAttachmentRequest struct {
	IdempotencyKey, MimeType, Name string
	Content                        []byte
}

const attachmentsDir = "attachments"

// SaveAttachment publishes complete bytes before the record that makes them
// resolvable. Keys bind the principal and session; names are display data only.
func (c *Catalog) SaveAttachment(ctx context.Context, sid string, req SaveAttachmentRequest) (store.Attachment, bool, error) {
	if err := c.fileWriteAllowed(ctx, sid); err != nil {
		return store.Attachment{}, false, err
	}
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 256 {
		return store.Attachment{}, false, product.NewError(product.CodeInvalidArgument, "idempotency key is required")
	}
	if len(req.Content) == 0 || len(req.Content) > config.WebAttachmentBytes {
		return store.Attachment{}, false, product.NewError(product.CodeInvalidArgument, "attachment size is outside its limit")
	}
	if err := validDisplayText(req.Name, config.WebAttachmentNameBytes); err != nil {
		return store.Attachment{}, false, err
	}
	if err := store.CheckAttachmentContent(req.MimeType, req.Content); err != nil {
		return store.Attachment{}, false, err
	}
	sum := sha256.Sum256(req.Content)
	rec := store.Attachment{ArtifactID: c.attachmentID(sid, req.IdempotencyKey), MimeType: req.MimeType, Name: req.Name, Size: int64(len(req.Content)), SHA256: hex.EncodeToString(sum[:])}
	rec.Digest = digestOf(rec.MimeType, rec.Name, rec.SHA256)
	c.files.Lock()
	defer c.files.Unlock()
	if err := c.beginFileWrite(ctx); err != nil {
		return store.Attachment{}, false, err
	}
	defer c.finishFileWrite()
	root, dir, err := c.sessionSubdir(sid, attachmentsDir, true)
	if err != nil {
		return store.Attachment{}, false, err
	}
	defer root.Close()
	if prior, err := store.ReadAttachmentRecord(root, rec.ArtifactID); err == nil {
		if prior.Digest != rec.Digest {
			return store.Attachment{}, false, product.NewError(product.CodeIdempotencyConflict, "key already belongs to a different attachment")
		}
		return prior, true, nil
	} else if e, ok := product.AsError(err); !ok || e.Code != product.CodeNotFound {
		return store.Attachment{}, false, err
	}
	n, err := countAttachments(root)
	if err != nil {
		return store.Attachment{}, false, err
	}
	if n >= config.WebAttachmentsPerSession {
		return store.Attachment{}, false, product.NewError(product.CodeBudgetExhausted, "attachment count limit reached")
	}
	if err = c.publish(root, dir, rec.ArtifactID+".bin", req.Content); err != nil {
		return store.Attachment{}, false, err
	}
	raw, _ := json.Marshal(rec)
	if err = ctx.Err(); err != nil {
		return store.Attachment{}, false, err
	}
	if err = c.publish(root, dir, rec.ArtifactID+".json", raw); err != nil {
		return store.Attachment{}, false, err
	}
	return rec, false, nil
}
func (c *Catalog) ReadAttachment(ctx context.Context, sid, aid string) (store.Attachment, []byte, error) {
	if err := c.fileReadAllowed(ctx, sid); err != nil {
		return store.Attachment{}, nil, err
	}
	rec, data, err := store.ReadSessionAttachment(c.opts.StateRoot, sid, aid)
	if err != nil {
		return store.Attachment{}, nil, err
	}
	return rec, data, ctx.Err()
}
func (c *Catalog) attachmentID(sid, key string) string {
	raw, _ := json.Marshal([]string{c.opts.Principal, sid, "attachment", key})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}
func digestOf(parts ...any) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func countAttachments(root *os.Root) (int, error) {
	f, err := root.Open(".")
	if err != nil {
		return 0, store.FileIO(err)
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return 0, store.FileIO(err)
	}
	n := 0
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && store.ValidAttachmentID(id) {
			n++
		}
	}
	return n, nil
}
func (c *Catalog) fileWriteAllowed(ctx context.Context, sid string) error {
	c.mu.Lock()
	err := c.usable()
	profile := c.opts.Profile
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if store.ValidateResourceID(sid) != nil || !c.served(ctx, sid) {
		return product.NewError(product.CodeNotFound, "session does not exist")
	}
	if profile != codeagent.ProfileMemory {
		return product.NewError(product.CodeResourceUnavailable, "default file and process backends are not available")
	}
	return nil
}
func (c *Catalog) fileReadAllowed(ctx context.Context, sid string) error {
	c.mu.Lock()
	err := c.usable()
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if store.ValidateResourceID(sid) != nil || !c.served(ctx, sid) {
		return product.NewError(product.CodeNotFound, "session does not exist")
	}
	return nil
}
func (c *Catalog) sessionSubdir(sid, sub string, create bool) (*os.Root, string, error) {
	return store.SessionSubdir(c.opts.StateRoot, sid, sub, create)
}

// publishFile syncs complete bytes before atomic publication, then syncs the
// directory. A failed publication leaves no resolvable partial file.
func publishFile(root *os.Root, dir, name string, data []byte) error {
	tmp := "." + name + "-" + rand.Text() + ".tmp"
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return store.FileIO(err)
	}
	published := false
	defer func() {
		if !published {
			_ = root.Remove(tmp)
		}
	}()
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return store.FileIO(err)
	}
	if info, err := root.Lstat(name); err == nil && !info.Mode().IsRegular() {
		return product.NewError(product.CodeStorageUnavailable, "session file has invalid type")
	}
	if err = root.Rename(tmp, name); err != nil {
		return store.FileIO(err)
	}
	published = true
	// dir is retained for explicit publication hooks as diagnostic information.
	// Default synchronization stays on the same directory used for publication.
	if err = store.SyncRoot(root); err != nil {
		return store.FileIO(err)
	}
	return nil
}
func validDisplayText(s string, max int) error {
	if len(s) > max || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return product.NewError(product.CodeInvalidArgument, "display text is invalid")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return product.NewError(product.CodeInvalidArgument, "display text is invalid")
		}
	}
	return nil
}
