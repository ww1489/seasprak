package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
)

func TestSessionAttachmentResolverVerifiedBoundedAndReadOnly(t *testing.T) {
	root, sid, aid := t.TempDir(), "attachment-session", "0123456789abcdef0123456789abcdef"
	dir, err := PrepareSessionDir(root, sid)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = ReadSessionAttachment(root, sid, aid); err == nil {
		t.Fatal("missing attachment accepted")
	}
	if _, err = os.Lstat(filepath.Join(dir, "attachments")); !os.IsNotExist(err) {
		t.Fatal("resolver created directory")
	}
	if err = os.Mkdir(filepath.Join(dir, "attachments"), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("attachment content")
	sum := sha256.Sum256(data)
	rec := Attachment{ArtifactID: aid, MimeType: "text/plain", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Digest: "fixture"}
	raw, _ := json.Marshal(rec)
	for name, b := range map[string][]byte{aid + ".json": raw, aid + ".bin": data} {
		if err = os.WriteFile(filepath.Join(dir, "attachments", name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, content, err := ReadSessionAttachment(root, sid, aid)
	if err != nil || got != rec || !bytes.Equal(content, data) {
		t.Fatalf("verified read=%+v %v", got, err)
	}
	other, err := PrepareSessionDir(root, "other-session")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ReadSessionAttachment(root, "other-session", aid)
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeNotFound {
		t.Fatalf("foreign error=%v", err)
	}
	if _, err = os.Lstat(filepath.Join(other, "attachments")); !os.IsNotExist(err) {
		t.Fatal("foreign resolver wrote")
	}
	if err = os.WriteFile(filepath.Join(dir, "attachments", aid+".bin"), []byte("tampered content!!"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err = ReadSessionAttachment(root, sid, aid)
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStorageUnavailable {
		t.Fatalf("integrity error=%v", err)
	}
	for name, want := range map[string][]byte{aid + ".json": raw, aid + ".bin": []byte("tampered content!!")} {
		got, e := os.ReadFile(filepath.Join(dir, "attachments", name))
		if e != nil || !bytes.Equal(got, want) {
			t.Fatal("resolver repaired or rewrote file")
		}
	}
}

func TestAttachmentContentAndIDValidation(t *testing.T) {
	for _, aid := range []string{"", "../outside", "0123456789ABCDEF0123456789abcdef"} {
		if ValidAttachmentID(aid) {
			t.Fatal("invalid id allowed")
		}
	}
	if !ValidAttachmentID("0123456789abcdef0123456789abcdef") {
		t.Fatal("valid id rejected")
	}
	for _, tc := range []struct {
		mime string
		data []byte
		code string
	}{
		{"text/html", []byte("<p>x</p>"), product.CodeUnsupportedCapability},
		{"image/png", []byte("not png"), product.CodeInvalidArgument},
		{"text/plain", []byte{0xff}, product.CodeInvalidArgument},
		{"application/json", []byte("{"), product.CodeInvalidArgument},
	} {
		if pe, ok := product.AsError(CheckAttachmentContent(tc.mime, tc.data)); !ok || pe.Code != tc.code {
			t.Fatalf("content type=%s err=%v", tc.mime, pe)
		}
	}
	if err := CheckAttachmentContent("text/plain", []byte("valid UTF-8 文本")); err != nil {
		t.Fatal(err)
	}
}
