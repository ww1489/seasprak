package storage

import (
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
)

// Attachment is a saved-material receipt. Its ID resolves only inside its own
// session. Name is display data, never a storage path or authorization ticket.
type Attachment struct {
	ArtifactID string `json:"artifactId"`
	MimeType   string `json:"mimeType"`
	Name       string `json:"name,omitempty"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Digest     string `json:"digest"`
}

func attachmentMimeAllowed(mime string) bool {
	switch mime {
	case "text/plain", "text/markdown", "application/json", "image/png", "image/jpeg":
		return true
	}
	return false
}
func ValidAttachmentID(id string) bool {
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
func CheckAttachmentContent(mimeType string, data []byte) error {
	if !attachmentMimeAllowed(mimeType) {
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

// ReadSessionAttachment is the neutral, verified resolver. It neither creates
// directories nor repairs records, and IDs never resolve in another session.
func ReadSessionAttachment(stateRoot, sid, aid string) (Attachment, []byte, error) {
	return readSessionAttachmentWithOpenRoot(stateRoot, sid, aid, nil)
}

func readSessionAttachmentWithOpenRoot(stateRoot, sid, aid string, openChild func(*os.Root, string) (*os.Root, error)) (Attachment, []byte, error) {
	if !ValidAttachmentID(aid) {
		return Attachment{}, nil, product.NewError(product.CodeNotFound, "attachment not found")
	}
	// Preserve the original lookup error classification. This path is never
	// used to open attachment files; all I/O below uses checked relative roots.
	if _, err := OpenSessionDir(stateRoot, sid); err != nil {
		return Attachment{}, nil, product.NewError(product.CodeNotFound, "session does not exist")
	}
	roots, err := OpenResourceRoots(stateRoot, ResourceCode, sid, false, openChild)
	if err != nil {
		return Attachment{}, nil, FileIO(err)
	}
	defer roots.Close()
	return readSessionAttachmentFromRoot(roots.Resource, aid, openChild)
}

// ReadSessionAttachmentFromRoot borrows an already bound session directory.
// It opens no other session, creates nothing, and does not close the borrowed root.
func ReadSessionAttachmentFromRoot(session *os.Root, aid string) (Attachment, []byte, error) {
	return readSessionAttachmentFromRoot(session, aid, nil)
}

func readSessionAttachmentFromRoot(session *os.Root, aid string, openChild func(*os.Root, string) (*os.Root, error)) (Attachment, []byte, error) {
	if !ValidAttachmentID(aid) {
		return Attachment{}, nil, product.NewError(product.CodeNotFound, "attachment not found")
	}
	root, err := OpenChildRoot(session, "attachments", false, openChild)
	if err != nil {
		return Attachment{}, nil, FileIO(err)
	}
	defer root.Close()
	rec, err := ReadAttachmentRecord(root, aid)
	if err != nil {
		return Attachment{}, nil, err
	}
	data, err := ReadRegular(root, aid+".bin", rec.Size)
	if err != nil {
		return Attachment{}, nil, err
	}
	if sum := sha256.Sum256(data); int64(len(data)) != rec.Size || hex.EncodeToString(sum[:]) != rec.SHA256 {
		return Attachment{}, nil, product.NewError(product.CodeStorageUnavailable, "attachment integrity check failed")
	}
	return rec, data, nil
}
func ReadAttachmentRecord(root *os.Root, aid string) (Attachment, error) {
	if !ValidAttachmentID(aid) {
		return Attachment{}, product.NewError(product.CodeNotFound, "attachment not found")
	}
	raw, err := ReadRegular(root, aid+".json", 64<<10)
	if err != nil {
		return Attachment{}, err
	}
	var rec Attachment
	if json.Unmarshal(raw, &rec) != nil || rec.ArtifactID != aid || !attachmentMimeAllowed(rec.MimeType) || rec.Size < 0 || rec.Size > config.WebAttachmentBytes {
		return Attachment{}, product.NewError(product.CodeStorageUnavailable, "attachment record is corrupt")
	}
	return rec, nil
}

// SessionSubdir opens only an existing protected session and one optional
// relative subdirectory. Creating the subdirectory is an explicit write action.
func SessionSubdir(stateRoot, sid, sub string, create bool) (*os.Root, string, error) {
	return sessionSubdirWithOpenRoot(stateRoot, sid, sub, create, nil)
}

// Public entry points use real checked relative opens. A single call may
// substitute only the relative child opener, never reopen a diagnostic path.
func sessionSubdirWithOpenRoot(stateRoot, sid, sub string, create bool, openChild func(*os.Root, string) (*os.Root, error)) (*os.Root, string, error) {
	if sub != "" && ValidateResourceID(sub) != nil {
		return nil, "", product.NewError(product.CodeInvalidArgument, "session subdirectory is invalid")
	}
	// Preserve the original lookup classification; actual I/O uses roots below.
	if _, err := OpenSessionDir(stateRoot, sid); err != nil {
		return nil, "", product.NewError(product.CodeNotFound, "session does not exist")
	}
	roots, err := OpenResourceRoots(stateRoot, ResourceCode, sid, false, openChild)
	if err != nil {
		return nil, "", FileIO(err)
	}
	defer roots.Close()
	if sub == "" {
		root := roots.Resource
		roots.Resource = nil // Transfer only this checked root to the caller.
		return root, roots.Path, nil
	}
	root, err := OpenChildRoot(roots.Resource, sub, create, openChild)
	if err != nil {
		return nil, "", FileIO(err)
	}
	if create {
		if err := SyncRoot(roots.Resource); err != nil {
			_ = root.Close()
			return nil, "", FileIO(err)
		}
	}
	return root, filepath.Join(roots.Path, sub), nil
}

// ReadRegular reads a non-link regular file with a strict byte bound and checks
// that the file inspected, opened and still named by the path is the same file.
func ReadRegular(root *os.Root, name string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, FileIO(err)
	}
	if !info.Mode().IsRegular() || IsReparseInfo(info) || info.Size() > limit {
		return nil, product.NewError(product.CodeStorageUnavailable, "session file has invalid type")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, FileIO(err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || IsReparseInfo(opened) || !os.SameFile(info, opened) {
		return nil, product.NewError(product.CodeStorageUnavailable, "session file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, FileIO(err)
	}
	if int64(len(data)) > limit {
		return nil, product.NewError(product.CodeStorageUnavailable, "session file exceeds limit")
	}
	current, err := root.Lstat(name)
	if err != nil || !current.Mode().IsRegular() || IsReparseInfo(current) || !os.SameFile(opened, current) {
		return nil, product.NewError(product.CodeStorageUnavailable, "session file changed while reading")
	}
	return data, nil
}
func FileIO(err error) error {
	if os.IsNotExist(err) {
		return product.NewError(product.CodeNotFound, "session file not found")
	}
	return product.NewError(product.CodeStorageUnavailable, "session file operation failed")
}
