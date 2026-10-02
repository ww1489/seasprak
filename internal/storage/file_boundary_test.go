package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	product "github.com/ww1489/seasprak/internal/errors"
)

// A channel stops one call after its checks and before the real relative child open.
// No global hook, abstract filesystem, sleeps, or probabilistic retry is used.
func TestFileBoundaryAttachmentRejectsDirectoryReplacement(t *testing.T) {
	for _, component := range []string{"sessions", "session", "attachments"} {
		t.Run(component, func(t *testing.T) {
			root, outside, sid, aid := t.TempDir(), t.TempDir(), "boundary-attachment", "0123456789abcdef0123456789abcdef"
			dir, err := PrepareSessionDir(root, sid)
			if err != nil {
				t.Fatal(err)
			}
			attachmentDir := filepath.Join(dir, "attachments")
			if err := os.Mkdir(attachmentDir, 0700); err != nil {
				t.Fatal(err)
			}
			fileBoundaryAttachment(t, attachmentDir, aid, []byte("trusted attachment"))
			link, externalDir := attachmentDir, outside
			switch component {
			case "sessions":
				link, externalDir = filepath.Dir(dir), filepath.Join(outside, sid, "attachments")
			case "session":
				link, externalDir = dir, filepath.Join(outside, "attachments")
			}
			if err := os.MkdirAll(externalDir, 0755); err != nil {
				t.Fatal(err)
			}
			fileBoundaryAttachment(t, externalDir, aid, []byte("external attachment"))
			before := fileBoundaryTree(t, outside)
			reached, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(resume) }) }
			type result struct {
				data []byte
				err  error
			}
			done := make(chan result, 1)
			finished, opens := false, 0
			target := component
			if component == "session" {
				target = sid
			}
			go func() {
				_, data, err := readSessionAttachmentWithOpenRoot(root, sid, aid, func(parent *os.Root, name string) (*os.Root, error) {
					if name == target {
						opens++
						close(reached)
						<-resume
					}
					return parent.OpenRoot(name)
				})
				done <- result{data, err}
			}()
			t.Cleanup(func() {
				unblock()
				if !finished {
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						t.Error("attachment opener did not exit")
					}
				}
			})
			select {
			case <-reached:
			case <-time.After(10 * time.Second):
				t.Fatal("attachment opener did not reach boundary")
			}
			if err := os.Rename(link, link+".held"); err != nil {
				t.Fatal("replace checked directory:", err)
			}
			storageDirectoryLink(t, link, outside)
			t.Cleanup(func() { _ = os.Remove(link) })
			unblock()
			var got result
			select {
			case got = <-done:
				finished = true
			case <-time.After(10 * time.Second):
				t.Fatal("attachment opener did not return")
			}
			if opens != 1 {
				t.Errorf("target relative directory open calls=%d, want 1", opens)
			}
			if pe, ok := product.AsError(got.err); !ok || pe.Code != product.CodeStorageUnavailable {
				t.Errorf("directory replacement error=%v, want storage_unavailable; returned=%q", got.err, got.data)
			}
			if got.data != nil {
				t.Error("replaced directory returned attachment bytes")
			}
			if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
				t.Error("attachment access changed external bytes, modes or entries")
			}
		})
	}
}

func fileBoundaryAttachment(t *testing.T, dir, aid string, data []byte) {
	t.Helper()
	sum := sha256.Sum256(data)
	rec := Attachment{ArtifactID: aid, MimeType: "text/plain", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Digest: "boundary fixture"}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{aid + ".json": raw, aid + ".bin": data} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0644); err != nil {
			t.Fatal(err)
		}
	}
}

type fileBoundaryEntry struct {
	Mode os.FileMode
	Data []byte
}

func fileBoundaryTree(t *testing.T, dir string) map[string]fileBoundaryEntry {
	t.Helper()
	out := map[string]fileBoundaryEntry{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		saved := fileBoundaryEntry{Mode: info.Mode()}
		if info.Mode().IsRegular() {
			saved.Data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		out[rel] = saved
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
