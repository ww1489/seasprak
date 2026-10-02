package codeagent

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

func inspectHeaderBytes(t *testing.T, sid string) []byte {
	t.Helper()
	raw, err := json.Marshal(storage.Header{RecordType: "header", FormatVersion: 1, SessionID: sid, Workspace: workspaceJSON(t.TempDir(), "gen-one")})
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func headerDirectoryLink(t *testing.T, link, target string) {
	t.Helper()
	if goruntime.GOOS == "windows" {
		cmd, err := exec.LookPath("cmd")
		if err != nil {
			t.Skip("cmd unavailable for junction fixture")
		}
		out, err := exec.Command(cmd, "/d", "/c", "mklink", "/J", link, target).CombinedOutput()
		if err != nil {
			t.Fatalf("junction fixture: %v %s", err, out)
		}
	} else if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestInspectSessionHeaderAndOpenPreserveDistinctLimitCodes(t *testing.T) {
	model := testkit.NewFake()
	opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "inspect-limit-pair", Model: model, Profile: ProfileMemory}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(opts.StateRoot, "sessions", opts.SessionID)
	journal := filepath.Join(dir, "journal.jsonl")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(bytes.SplitN(before, []byte("\n"), 2)[0]) <= 64 {
		t.Fatal("fixture header must exceed the shared 64-byte limit")
	}
	// Remove the released writer's file so either query creating one is visible.
	lockPath := filepath.Join(dir, "writer.lock")
	if err = os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	opts.Limits.MaxCommitLineBytes = 64
	for _, tc := range []struct {
		name string
		code string
		read func() error
	}{
		{"OpenAgentSession", product.CodeInvalidArgument, func() error {
			opened, err := OpenAgentSession(t.Context(), opts)
			if opened != nil {
				t.Error("oversized header started a session")
				if closeErr := opened.Close(t.Context()); closeErr != nil {
					t.Errorf("unexpected session close: %v", closeErr)
				}
			}
			return err
		}},
		{"InspectSessionHeader", product.CodeIncompatibleVersion, func() error {
			_, err := InspectSessionHeader(t.Context(), opts.StateRoot, opts.SessionID, opts.Limits)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.read()
			if pe, ok := product.AsError(err); !ok || pe.Code != tc.code {
				t.Errorf("header limit error=%v, want code %s", err, tc.code)
			}
			after, err := os.ReadFile(journal)
			if err != nil || !bytes.Equal(before, after) {
				t.Error("header rejection changed journal bytes")
			}
			if calls := model.Calls(); calls != 0 {
				t.Errorf("header rejection invoked model %d times", calls)
			}
			if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
				t.Errorf("header rejection created writer lock: %v", err)
			}
		})
	}
}

func TestReadHeaderRejectsLinkedSessionPathWithoutWriting(t *testing.T) {
	for _, component := range []string{"sessions", "session"} {
		t.Run(component, func(t *testing.T) {
			root, outside, sid := t.TempDir(), t.TempDir(), "inspect-one"
			raw := inspectHeaderBytes(t, sid)
			journal := filepath.Join(outside, "journal.jsonl")
			link := filepath.Join(root, "sessions", sid)
			if component == "sessions" {
				if err := os.Mkdir(filepath.Join(outside, sid), 0700); err != nil {
					t.Fatal(err)
				}
				journal = filepath.Join(outside, sid, "journal.jsonl")
				link = filepath.Join(root, "sessions")
			} else if err := os.Mkdir(filepath.Join(root, "sessions"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(journal, raw, 0600); err != nil {
				t.Fatal(err)
			}
			headerDirectoryLink(t, link, outside)
			_, err := readHeader(root, sid, 1<<20)
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
				t.Errorf("linked header path error=%v", err)
			}
			_, err = InspectSessionHeader(t.Context(), root, sid, config.Limits{})
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
				t.Errorf("linked descriptor path error=%v", err)
			}
			got, e := os.ReadFile(journal)
			if e != nil || !bytes.Equal(got, raw) {
				t.Fatal("inspection changed outside journal")
			}
		})
	}
}

func TestReadHeaderRejectsLinkedJournalWithoutWriting(t *testing.T) {
	root, sid := t.TempDir(), "inspect-one"
	dir, err := storage.PrepareSessionDir(root, sid)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	raw := inspectHeaderBytes(t, sid)
	if err = os.WriteFile(outside, raw, 0600); err != nil {
		t.Fatal(err)
	}
	fileBoundaryFileLink(t, filepath.Join(dir, "journal.jsonl"), outside)
	if _, err = readHeader(root, sid, 1<<20); err == nil {
		t.Error("linked journal accepted")
	}
	got, err := os.ReadFile(outside)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("inspection wrote outside file")
	}
}

func TestReadHeaderRejectsNonregularAndBoundedHeaders(t *testing.T) {
	root, sid := t.TempDir(), "inspect-one"
	dir, err := storage.PrepareSessionDir(root, sid)
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(dir, "journal.jsonl")
	if err = os.Mkdir(journal, 0700); err != nil {
		t.Fatal(err)
	}
	_, err = readHeader(root, sid, 256)
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
		t.Errorf("nonregular header error=%v", err)
	}
	if err = os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	raw := inspectHeaderBytes(t, sid)
	if err = os.WriteFile(journal, raw, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = readHeader(root, sid, 8)
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
		t.Errorf("oversize Open header error=%v", err)
	}
	_, err = InspectSessionHeader(t.Context(), root, sid, config.Limits{MaxCommitLineBytes: 8})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
		t.Errorf("oversize Inspect header error=%v", err)
	}
	got, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("bounded inspection wrote journal")
	}
}
