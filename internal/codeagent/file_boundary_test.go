package codeagent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"encoding/json"

	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

// A valid external manifest must not supply a session's executable generation.
func TestFileBoundaryHeaderRejectsDirectoryReplacement(t *testing.T) {
	for _, replacement := range []string{"link", "ordinary"} {
		for _, component := range []string{"sessions", "session"} {
			t.Run(replacement+"/"+component, func(t *testing.T) {
				root, outside, sid := t.TempDir(), t.TempDir(), "boundary-header"
				dir, err := store.PrepareSessionDir(root, sid)
				if err != nil {
					t.Fatal(err)
				}
				header := store.Header{RecordType: "header", FormatVersion: 1, ResourceType: store.ResourceCode, SessionID: sid, Workspace: json.RawMessage(`{"fixture":"trusted"}`)}
				raw, err := json.Marshal(header)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "journal.jsonl"), append(raw, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
				path, externalDir := dir, outside
				if component == "sessions" {
					path, externalDir = filepath.Dir(dir), filepath.Join(outside, sid)
					if err := os.Mkdir(externalDir, 0755); err != nil {
						t.Fatal(err)
					}
				}
				header.Workspace = json.RawMessage(`{"fixture":"external"}`)
				raw, err = json.Marshal(header)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(externalDir, "journal.jsonl"), append(raw, '\n'), 0644); err != nil {
					t.Fatal(err)
				}
				before := fileBoundaryTree(t, outside)
				reached, resume := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(resume) }) }
				type result struct {
					header store.Header
					err    error
				}
				done := make(chan result, 1)
				finished, opens := false, 0
				go func() {
					got, err := readHeaderWithOpenRoot(root, sid, 4096, product.CodeInvalidArgument, func(parent *os.Root, name string) (*os.Root, error) {
						target := "sessions"
						if component == "session" {
							target = sid
						}
						if name == target {
							opens++
							close(reached)
							<-resume
						}
						return parent.OpenRoot(name)
					})
					done <- result{got, err}
				}()
				t.Cleanup(func() {
					unblock()
					if !finished {
						select {
						case <-done:
						case <-time.After(10 * time.Second):
							t.Error("header opener did not exit")
						}
					}
				})
				select {
				case <-reached:
				case <-time.After(10 * time.Second):
					t.Fatal("header opener did not reach boundary")
				}
				if err := os.Rename(path, path+".held"); err != nil {
					t.Fatal("replace checked directory:", err)
				}
				if replacement == "link" {
					fileBoundaryDirectoryLink(t, path, outside)
				} else {
					if err := os.Rename(outside, path); err != nil {
						t.Fatal("ordinary directory replacement:", err)
					}
					outside = path
					info, err := os.Lstat(path)
					linked, linkErr := store.IsReparse(path)
					if err != nil || linkErr != nil || !info.IsDir() || linked {
						t.Fatal("ordinary replacement must be a non-link directory", err, linkErr)
					}
				}
				unblock()
				var got result
				select {
				case got = <-done:
					finished = true
				case <-time.After(10 * time.Second):
					t.Fatal("header opener did not return")
				}
				if opens != 1 {
					t.Errorf("target directory open calls=%d, want 1", opens)
				}
				if pe, ok := product.AsError(got.err); !ok || pe.Code != product.CodeIncompatibleVersion {
					t.Errorf("directory replacement error=%v, want incompatible_version; header=%s", got.err, got.header.Workspace)
				}
				if !reflect.DeepEqual(got.header, store.Header{}) {
					t.Error("directory replacement returned an untrusted header")
				}
				if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
					t.Error("header read changed replacement bytes, modes or entries")
				}
			})
		}
	}
}

func TestFileBoundaryManifestRejectsStaticLinks(t *testing.T) {
	for _, component := range []string{"manifest.json", "generation", "resources"} {
		t.Run(component, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			model := testkit.NewFake()
			var toolCalls atomic.Int32
			opts := Options{
				Workspace: t.TempDir(), StateRoot: root, SessionID: "boundary-manifest", Profile: ProfileMemory, Model: model,
				Tools: []tools.Definition{{Name: "boundary", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
					toolCalls.Add(1)
					return "unused", nil
				}}},
			}
			s, err := CreateAgentSession(context.Background(), opts)
			if err != nil {
				t.Fatal("create fixture:", err)
			}
			generation := s.rt.generation
			if err := s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			resourceDir := filepath.Join(root, "sessions", opts.SessionID, "resources")
			generationDir := filepath.Join(resourceDir, generation)
			manifestPath := filepath.Join(generationDir, "manifest.json")
			raw, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			link, target := manifestPath, filepath.Join(outside, "manifest.json")
			if component == "generation" {
				link, target = generationDir, outside
			} else if component == "resources" {
				link, target = resourceDir, outside
				if err := os.Mkdir(filepath.Join(outside, generation), 0755); err != nil {
					t.Fatal(err)
				}
			}
			externalManifest := filepath.Join(target, "manifest.json")
			if component == "manifest.json" {
				externalManifest = target
			} else if component == "resources" {
				externalManifest = filepath.Join(target, generation, "manifest.json")
			}
			if err := os.WriteFile(externalManifest, raw, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(link, link+".held"); err != nil {
				t.Fatal(err)
			}
			if component == "manifest.json" {
				fileBoundaryFileLink(t, link, target)
			} else {
				fileBoundaryDirectoryLink(t, link, target)
			}
			before := fileBoundaryTree(t, outside)
			opts.ReadOnly = true
			browser, err := OpenAgentSession(context.Background(), opts)
			if err != nil {
				t.Fatal("read-only browse must remain available:", err)
			}
			if len(browser.rt.opts.Tools) != 0 || len(browser.rt.opts.ToolInfos) != 0 {
				t.Error("read-only browse retained tool capability from linked manifest")
			}
			if err := browser.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			opts.ReadOnly = false
			writer, openErr := OpenAgentSession(context.Background(), opts)
			if writer != nil {
				if err := writer.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if pe, ok := product.AsError(openErr); !ok || pe.Code != product.CodeIncompatibleVersion {
				t.Errorf("linked manifest writer error=%v, want incompatible_version", openErr)
			}
			if model.Calls() != 0 || toolCalls.Load() != 0 {
				t.Errorf("open executed work: model=%d tool=%d", model.Calls(), toolCalls.Load())
			}
			if after := fileBoundaryTree(t, outside); !reflect.DeepEqual(before, after) {
				t.Error("manifest access changed external bytes, modes or entries")
			}
		})
	}
}

func fileBoundaryFileLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if goruntime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
			t.Skip("Windows file symlink privilege unavailable; file-link behavior unverified")
		}
		t.Fatal("file symlink fixture:", err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
}

func fileBoundaryDirectoryLink(t *testing.T, link, target string) {
	t.Helper()
	if goruntime.GOOS == "windows" {
		command, err := exec.LookPath("cmd.exe")
		if err != nil {
			t.Skip("cmd.exe unavailable; directory junction behavior unverified")
		}
		if out, err := exec.Command(command, "/d", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Fatalf("directory junction fixture: %v: %s", err, out)
		}
	} else if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
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
			saved.Data = bytes.Clone(saved.Data)
		}
		out[rel] = saved
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
