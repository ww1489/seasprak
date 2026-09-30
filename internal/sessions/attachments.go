package sessions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	sessstore "github.com/ww1489/seasprak/internal/sessions/store"
)

// Attachment is the saved-material receipt. ArtifactID resolves only inside
// its own session's attachment directory; Name is display data and is never
// used as a storage path. Saving an attachment never triggers a model.
type Attachment struct {
	ArtifactID string `json:"artifactId"`
	MimeType   string `json:"mimeType"`
	Name       string `json:"name,omitempty"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	// Digest binds the idempotency key to the request content.
	Digest string `json:"digest"`
}

// SaveAttachmentRequest is one bounded upload. Content is the raw bytes.
type SaveAttachmentRequest struct {
	IdempotencyKey string
	MimeType       string
	Name           string
	Content        []byte
}

// AttachmentMimeTypes is the fixed allowlist of storable media types.
var attachmentMimeTypes = map[string]bool{
	"text/plain": true, "text/markdown": true, "application/json": true,
	"image/png": true, "image/jpeg": true,
}

const attachmentsDir = "attachments"

// SaveAttachment durably publishes the bytes, then the record that makes the
// artifact ID resolvable. The same key with the same content replays the
// original receipt; different content under that key is a conflict.
func (c *Catalog) SaveAttachment(ctx context.Context, sid string, req SaveAttachmentRequest) (Attachment, bool, error) {
	if err := c.fileWriteAllowed(sid); err != nil {
		return Attachment{}, false, err
	}
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 256 {
		return Attachment{}, false, product.NewError(product.CodeInvalidArgument, "idempotency key is required")
	}
	if len(req.Content) == 0 || len(req.Content) > config.WebAttachmentBytes {
		return Attachment{}, false, product.NewError(product.CodeInvalidArgument, "attachment size is outside its limit")
	}
	if err := validDisplayText(req.Name, config.WebAttachmentNameBytes); err != nil {
		return Attachment{}, false, err
	}
	if err := checkAttachmentContent(req.MimeType, req.Content); err != nil {
		return Attachment{}, false, err
	}
	sum := sha256.Sum256(req.Content)
	rec := Attachment{ArtifactID: c.attachmentID(sid, req.IdempotencyKey), MimeType: req.MimeType, Name: req.Name, Size: int64(len(req.Content)), SHA256: hex.EncodeToString(sum[:])}
	rec.Digest = digestOf(rec.MimeType, rec.Name, rec.SHA256)

	c.files.Lock()
	defer c.files.Unlock()
	if err := ctx.Err(); err != nil {
		return Attachment{}, false, err
	}
	root, dir, err := c.sessionSubdir(sid, attachmentsDir, true)
	if err != nil {
		return Attachment{}, false, err
	}
	defer root.Close()
	if prior, err := readAttachmentRecord(root, rec.ArtifactID); err == nil {
		if prior.Digest != rec.Digest {
			return Attachment{}, false, product.NewError(product.CodeIdempotencyConflict, "key already belongs to a different attachment")
		}
		return prior, true, nil
	} else if e, ok := product.AsError(err); !ok || e.Code != product.CodeNotFound {
		return Attachment{}, false, err
	}
	n, err := countAttachments(root)
	if err != nil {
		return Attachment{}, false, err
	}
	if n >= config.WebAttachmentsPerSession {
		return Attachment{}, false, product.NewError(product.CodeBudgetExhausted, "attachment count limit reached")
	}
	// Bytes first, record last: a crash between them leaves an unreachable
	// orphan, never an artifact ID without complete content.
	if err = publishFile(root, dir, rec.ArtifactID+".bin", req.Content); err != nil {
		return Attachment{}, false, err
	}
	raw, _ := json.Marshal(rec)
	if err = ctx.Err(); err != nil {
		return Attachment{}, false, err
	}
	if err = publishFile(root, dir, rec.ArtifactID+".json", raw); err != nil {
		return Attachment{}, false, err
	}
	return rec, false, nil
}

// ReadAttachment returns the verified complete content. It never writes.
func (c *Catalog) ReadAttachment(ctx context.Context, sid, aid string) (Attachment, []byte, error) {
	if err := c.fileReadAllowed(ctx, sid); err != nil {
		return Attachment{}, nil, err
	}
	if !validAttachmentID(aid) {
		return Attachment{}, nil, product.NewError(product.CodeNotFound, "attachment not found")
	}
	rec, data, err := readSessionAttachment(c.opts.StateRoot, sid, aid)
	if err != nil {
		return Attachment{}, nil, err
	}
	return rec, data, ctx.Err()
}

// readSessionAttachment reads and verifies one attachment of sid. It is the
// only resolver: IDs never escape their own session directory.
func readSessionAttachment(stateRoot, sid, aid string) (Attachment, []byte, error) {
	if !validAttachmentID(aid) {
		return Attachment{}, nil, product.NewError(product.CodeNotFound, "attachment not found")
	}
	root, _, err := sessionSubdirAt(stateRoot, sid, attachmentsDir, false)
	if err != nil {
		return Attachment{}, nil, err
	}
	defer root.Close()
	rec, err := readAttachmentRecord(root, aid)
	if err != nil {
		return Attachment{}, nil, err
	}
	data, err := readRegular(root, aid+".bin", rec.Size)
	if err != nil {
		return Attachment{}, nil, err
	}
	if sum := sha256.Sum256(data); int64(len(data)) != rec.Size || hex.EncodeToString(sum[:]) != rec.SHA256 {
		return Attachment{}, nil, product.NewError(product.CodeStorageUnavailable, "attachment integrity check failed")
	}
	return rec, data, nil
}

// checkAttachmentContent enforces the allowlist and requires sniffed content
// to agree with the declared type.
func checkAttachmentContent(mimeType string, data []byte) error {
	if !attachmentMimeTypes[mimeType] {
		return product.NewError(product.CodeUnsupportedCapability, "attachment media type is not allowed")
	}
	sniffed := http.DetectContentType(data)
	switch mimeType {
	case "image/png", "image/jpeg":
		if sniffed != mimeType {
			return product.NewError(product.CodeInvalidArgument, "attachment content does not match its media type")
		}
	default:
		if !strings.HasPrefix(sniffed, "text/plain") || !utf8.Valid(data) {
			return product.NewError(product.CodeInvalidArgument, "attachment content does not match its media type")
		}
		if mimeType == "application/json" && !json.Valid(data) {
			return product.NewError(product.CodeInvalidArgument, "attachment content does not match its media type")
		}
	}
	return nil
}

// attachmentID derives a stable ID from the principal, session and key so a
// lost response can be replayed without an index. It is not a path.
func (c *Catalog) attachmentID(sid, key string) string {
	raw, _ := json.Marshal([]string{c.opts.Principal, sid, "attachment", key})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

func validAttachmentID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func digestOf(parts ...any) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func readAttachmentRecord(root *os.Root, aid string) (Attachment, error) {
	raw, err := readRegular(root, aid+".json", 64<<10)
	if err != nil {
		return Attachment{}, err
	}
	var rec Attachment
	if json.Unmarshal(raw, &rec) != nil || rec.ArtifactID != aid || !attachmentMimeTypes[rec.MimeType] || rec.Size < 0 || rec.Size > config.WebAttachmentBytes {
		return Attachment{}, product.NewError(product.CodeStorageUnavailable, "attachment record is corrupt")
	}
	return rec, nil
}

func countAttachments(root *os.Root) (int, error) {
	f, err := root.Open(".")
	if err != nil {
		return 0, fileIO(err)
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return 0, fileIO(err)
	}
	n := 0
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && validAttachmentID(id) {
			n++
		}
	}
	return n, nil
}

// fileWriteAllowed mirrors Writer: the session must be served here, and the
// default profile writes nothing.
func (c *Catalog) fileWriteAllowed(sid string) error {
	c.mu.Lock()
	err := c.usable()
	profile := c.opts.Profile
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if sessstore.ValidateResourceID(sid) != nil || !c.served(sid) {
		return product.NewError(product.CodeNotFound, "session does not exist")
	}
	if profile != ProfileMemory {
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
	if sessstore.ValidateResourceID(sid) != nil || !c.served(sid) {
		return product.NewError(product.CodeNotFound, "session does not exist")
	}
	return nil
}

// sessionSubdir opens sessions/<sid>[/sub] as an os.Root after rejecting
// symlinks and junctions on every component. An empty sub opens the session
// directory. Without create, a missing directory is not_found.
func (c *Catalog) sessionSubdir(sid, sub string, create bool) (*os.Root, string, error) {
	return sessionSubdirAt(c.opts.StateRoot, sid, sub, create)
}

func sessionSubdirAt(stateRoot, sid, sub string, create bool) (*os.Root, string, error) {
	dir, err := sessstore.OpenSessionDir(stateRoot, sid)
	if err != nil {
		return nil, "", product.NewError(product.CodeNotFound, "session does not exist")
	}
	if sub != "" {
		dir = filepath.Join(dir, sub)
		if create {
			if err = os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
				return nil, "", fileIO(err)
			}
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return nil, "", fileIO(err)
		}
		if reparse, err := sessstore.IsReparse(dir); err != nil || reparse || !info.IsDir() {
			return nil, "", product.NewError(product.CodeStorageUnavailable, "session file directory has invalid type")
		}
		if create {
			if err = sessstore.SyncDir(filepath.Dir(dir)); err != nil {
				return nil, "", fileIO(err)
			}
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", fileIO(err)
	}
	return root, dir, nil
}

// publishFile writes a synced temporary file and renames it into place, then
// syncs the directory. The name becomes visible only with complete content.
func publishFile(root *os.Root, dir, name string, data []byte) error {
	tmp := "." + name + "-" + rand.Text() + ".tmp"
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fileIO(err)
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
		return fileIO(err)
	}
	if info, err := root.Lstat(name); err == nil && !info.Mode().IsRegular() {
		return product.NewError(product.CodeStorageUnavailable, "session file has invalid type")
	}
	if err = root.Rename(tmp, name); err != nil {
		return fileIO(err)
	}
	published = true
	if err = sessstore.SyncDir(dir); err != nil {
		return fileIO(err)
	}
	return nil
}

// readRegular reads a regular, non-link file of at most limit bytes.
func readRegular(root *os.Root, name string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, fileIO(err)
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, product.NewError(product.CodeStorageUnavailable, "session file has invalid type")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, fileIO(err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, product.NewError(product.CodeStorageUnavailable, "session file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fileIO(err)
	}
	if int64(len(data)) > limit {
		return nil, product.NewError(product.CodeStorageUnavailable, "session file exceeds limit")
	}
	return data, nil
}

// validDisplayText accepts bounded UTF-8 without control characters.
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

func fileIO(err error) error {
	if os.IsNotExist(err) {
		return product.NewError(product.CodeNotFound, "session file not found")
	}
	return product.NewError(product.CodeStorageUnavailable, "session file operation failed")
}
