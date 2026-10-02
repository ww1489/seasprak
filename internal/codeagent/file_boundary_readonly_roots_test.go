package codeagent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

func TestFileBoundaryHeaderFromRootBorrowsCompletedBinding(t *testing.T) {
	state, outside, sid := t.TempDir(), t.TempDir(), "bound-header"
	path, err := store.PrepareSessionDir(state, sid)
	if err != nil {
		t.Fatal(err)
	}
	header := store.Header{RecordType: "header", FormatVersion: 1, ResourceType: store.ResourceCode, SessionID: sid, Workspace: json.RawMessage(`{"fixture":"trusted"}`)}
	raw, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "journal.jsonl"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	roots, err := store.OpenResourceRoots(state, store.ResourceCode, sid, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	if err := os.WriteFile(filepath.Join(outside, "journal.jsonl"), []byte("external"), 0644); err != nil {
		t.Fatal(err)
	}
	before := fileBoundaryTree(t, outside)
	if err := os.Rename(path, path+".held"); err != nil {
		t.Fatal(err)
	}
	fileBoundaryDirectoryLink(t, path, outside)
	got, err := readHeaderFromRoot(roots.Resource, sid, 0, product.CodeInvalidArgument)
	if err != nil || !reflect.DeepEqual(got, header) {
		t.Fatalf("borrowed header=%+v error=%v", got, err)
	}
	for _, limitCode := range []string{product.CodeInvalidArgument, product.CodeIncompatibleVersion} {
		got, err := readHeaderFromRoot(roots.Resource, sid, 8, limitCode)
		fileBoundaryReadError(t, err, limitCode, state, outside)
		if !reflect.DeepEqual(got, store.Header{}) {
			t.Error("over-limit read returned a header")
		}
	}
	got, err = readHeaderFromRoot(roots.Resource, "different", 4096, product.CodeInvalidArgument)
	fileBoundaryReadError(t, err, product.CodeIncompatibleVersion, state, outside)
	if !reflect.DeepEqual(got, store.Header{}) {
		t.Error("identity mismatch returned a header")
	}
	if _, err := roots.Resource.Stat("."); err != nil {
		t.Fatal("header core closed its borrowed root:", err)
	}
	if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
		t.Error("borrowed header access changed external bytes, modes or entries")
	}
}

func TestFileBoundaryHeaderMapsDirectoryIOWithoutPaths(t *testing.T) {
	state, sid := t.TempDir(), "bound-header"
	if _, err := store.PrepareSessionDir(state, sid); err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err := readHeaderWithOpenRoot(state, sid, 4096, product.CodeInvalidArgument, func(parent *os.Root, name string) (*os.Root, error) {
		calls++
		return nil, &os.PathError{Op: "open", Path: state, Err: errors.New("unavailable")}
	})
	fileBoundaryReadError(t, err, product.CodeStorageUnavailable, state)
	if calls != 1 {
		t.Errorf("failed directory opener calls=%d, want 1", calls)
	}
}

func TestFileBoundaryManifestRejectsOrdinaryIdentityReplacement(t *testing.T) {
	for _, component := range []string{"resources", "generation", "manifest.json"} {
		for _, phase := range []string{"before-open", "after-open"} {
			t.Run(component+"/"+phase, func(t *testing.T) {
				state, outside, sid := t.TempDir(), t.TempDir(), "bound-manifest"
				manifest := sampleManifest(t)
				if err := saveManifest(state, sid, manifest); err != nil {
					t.Fatal(err)
				}
				roots, err := store.OpenResourceRoots(state, store.ResourceCode, sid, false, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer roots.Close()
				path := filepath.Join(roots.Path, "resources")
				replacement, externalManifest := outside, filepath.Join(outside, "manifest.json")
				target := "resources"
				if component == "resources" {
					if err := os.Mkdir(filepath.Join(outside, manifest.ID), 0755); err != nil {
						t.Fatal(err)
					}
					externalManifest = filepath.Join(outside, manifest.ID, "manifest.json")
				} else {
					path, target = filepath.Join(path, manifest.ID), manifest.ID
					if component == "manifest.json" {
						path, replacement = filepath.Join(path, "manifest.json"), externalManifest
					}
				}
				raw, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(externalManifest, raw, 0644); err != nil {
					t.Fatal(err)
				}
				before := fileBoundaryTree(t, replacement)
				opens := 0
				swap := func() {
					if err := os.Rename(path, path+".held"); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(replacement, path); err != nil {
						t.Fatal(err)
					}
					info, err := os.Lstat(path)
					if err != nil || store.IsReparseInfo(info) || component == "manifest.json" && !info.Mode().IsRegular() || component != "manifest.json" && !info.IsDir() {
						t.Fatal("ordinary replacement has invalid type", err)
					}
				}
				openChild := func(parent *os.Root, name string) (*os.Root, error) {
					if component == "manifest.json" || name != target {
						return parent.OpenRoot(name)
					}
					opens++
					if phase == "before-open" {
						swap()
						return parent.OpenRoot(name)
					}
					child, err := parent.OpenRoot(name)
					if err == nil {
						swap()
					}
					return child, err
				}
				openFile := func(parent *os.Root, name string) (*os.File, error) {
					if component != "manifest.json" {
						return parent.Open(name)
					}
					opens++
					if phase == "before-open" {
						swap()
						return parent.Open(name)
					}
					file, err := parent.Open(name)
					if err == nil {
						swap()
					}
					return file, err
				}
				got, err := loadManifestFromRootWithOpen(roots.Resource, manifest.ID, openChild, openFile)
				fileBoundaryReadError(t, err, product.CodeIncompatibleVersion, state, outside)
				if pe, ok := product.AsError(err); !ok || pe.Message != "generation manifest is unreadable" {
					t.Error("identity replacement changed the fixed manifest error")
				}
				if !reflect.DeepEqual(got, capabilityManifest{}) || opens != 1 {
					t.Errorf("identity replacement returned a manifest or missed boundary: manifest=%+v opens=%d", got, opens)
				}
				if !reflect.DeepEqual(before, fileBoundaryTree(t, path)) {
					t.Error("manifest access changed replacement bytes, modes or entries")
				}
				if _, err := roots.Resource.Stat("."); err != nil {
					t.Fatal("manifest core closed its borrowed root:", err)
				}
			})
		}
	}
}

func TestFileBoundaryManifestFromRootAndGenerationBorrowBinding(t *testing.T) {
	state, outside, sid := t.TempDir(), t.TempDir(), "bound-manifest"
	manifest, err := buildManifest("", nil, "bound-model")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveManifest(state, sid, manifest); err != nil {
		t.Fatal(err)
	}
	roots, err := store.OpenResourceRoots(state, store.ResourceCode, sid, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	if err := os.Rename(roots.Path, roots.Path+".held"); err != nil {
		t.Fatal(err)
	}
	fileBoundaryDirectoryLink(t, roots.Path, outside)
	got, err := loadManifestFromRoot(roots.Resource, manifest.ID)
	if err != nil || !reflect.DeepEqual(got, manifest) {
		t.Fatalf("borrowed manifest=%+v error=%v", got, err)
	}
	opts := Options{StateRoot: outside, SessionID: sid, fileRoot: roots.Resource, GenerationFingerprint: manifest.Model}
	if err := matchGeneration(&opts, manifest.ID); err != nil {
		t.Fatal("generation matching did not use borrowed root:", err)
	}
	opts.fileRoot = nil
	fileBoundaryReadError(t, matchGeneration(&opts, manifest.ID), product.CodeIncompatibleVersion, outside)
	if _, err := roots.Resource.Stat("."); err != nil {
		t.Fatal("manifest core closed its borrowed root:", err)
	}
}

func TestFileBoundaryManifestMapsMissingTypesAndIO(t *testing.T) {
	for _, component := range []string{"resources", "generation", "manifest.json"} {
		for _, failure := range []string{"missing", "wrong-type", "open-io"} {
			t.Run(component+"/"+failure, func(t *testing.T) {
				state, sid := t.TempDir(), "bound-manifest"
				manifest := sampleManifest(t)
				if err := saveManifest(state, sid, manifest); err != nil {
					t.Fatal(err)
				}
				roots, err := store.OpenResourceRoots(state, store.ResourceCode, sid, false, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer roots.Close()
				path, target := filepath.Join(roots.Path, "resources"), "resources"
				if component != "resources" {
					path, target = filepath.Join(path, manifest.ID), manifest.ID
					if component == "manifest.json" {
						path = filepath.Join(path, component)
					}
				}
				if failure != "open-io" {
					if err := os.RemoveAll(path); err != nil {
						t.Fatal(err)
					}
					if failure == "wrong-type" {
						if component == "manifest.json" {
							err = os.Mkdir(path, 0700)
						} else {
							err = os.WriteFile(path, []byte("not a directory"), 0600)
						}
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				injected := &os.PathError{Op: "open", Path: state, Err: errors.New("unavailable")}
				openChild := func(parent *os.Root, name string) (*os.Root, error) {
					if failure == "open-io" && component != "manifest.json" && name == target {
						return nil, injected
					}
					return parent.OpenRoot(name)
				}
				openFile := func(parent *os.Root, name string) (*os.File, error) {
					if failure == "open-io" && component == "manifest.json" {
						return nil, injected
					}
					return parent.Open(name)
				}
				got, err := loadManifestFromRootWithOpen(roots.Resource, manifest.ID, openChild, openFile)
				fileBoundaryReadError(t, err, product.CodeIncompatibleVersion, state)
				message := "generation manifest is unreadable"
				if failure == "missing" {
					message = "generation manifest is missing"
				}
				if pe, ok := product.AsError(err); !ok || pe.Message != message {
					t.Error("manifest failure changed its fixed public error")
				}
				if !reflect.DeepEqual(got, capabilityManifest{}) {
					t.Error("invalid manifest returned capabilities")
				}
			})
		}
	}
}

func fileBoundaryReadError(t *testing.T, err error, code string, paths ...string) {
	t.Helper()
	if pe, ok := product.AsError(err); !ok || pe.Code != code {
		t.Errorf("error=%v, want %s", err, code)
	}
	if err != nil {
		for _, path := range paths {
			if strings.Contains(err.Error(), path) {
				t.Error("public read error exposed a raw path")
			}
		}
	}
}
