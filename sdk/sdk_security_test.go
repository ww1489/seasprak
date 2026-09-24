package sdk_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/gofrs/flock"

	"github.com/ww1489/seasprak/internal/testkit"
	"github.com/ww1489/seasprak/sdk"
)

func TestStateRootInsideWorkspaceRejected(t *testing.T) {
	ws := t.TempDir()
	state := filepath.Join(ws, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := sdk.CreateAgentSession(context.Background(), memoryOpts(t, ws, state, "sess-nested"))
	expectCode(t, err, sdk.CodeInvalidArgument)
}

func TestSymlinkDoesNotHideStateOverlap(t *testing.T) {
	realWS := t.TempDir()
	state := filepath.Join(realWS, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "ws-link")
	if err := os.Symlink(realWS, link); err != nil {
		t.Skip(err)
	}
	_, err := sdk.CreateAgentSession(context.Background(), memoryOpts(t, link, state, "sess-link"))
	expectCode(t, err, sdk.CodeInvalidArgument)
}

func TestRelativeWorkspaceStoredAsRealPath(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	t.Chdir(filepath.Dir(ws))
	opts := memoryOpts(t, filepath.Base(ws), state, "sess-rel")
	s, err := sdk.CreateAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)
	got := hostRealRoot(t, state, "sess-rel")
	want, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(got, want) {
		t.Fatalf("workspace binding %q, want real path %q", got, want)
	}
}

func TestSessionIDAllowsSingleSegmentAndRejectsEscape(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	s, err := sdk.CreateAgentSession(context.Background(), memoryOpts(t, ws, state, "sess-reopen"))
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)
	for _, id := range []string{"..", "../outside", "a/b", "a:b", "CON", "com1", "NUL.txt"} {
		_, err := sdk.CreateAgentSession(context.Background(), memoryOpts(t, ws, state, id))
		expectCode(t, err, sdk.CodeInvalidArgument)
	}
}

func TestDefaultStateRootIsUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AppData", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	ws := t.TempDir()
	opts := memoryOpts(t, ws, "", "sess-default-root")
	opts.StateRoot = ""
	s, err := sdk.CreateAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)
	journal := filepath.Join(home, "seasprak", "state", "sessions", "sess-default-root", "journal.jsonl")
	if _, err := os.Stat(journal); err != nil {
		t.Fatalf("default state journal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(os.TempDir(), "seasprak-state", "sessions", "sess-default-root")); !os.IsNotExist(err) {
		t.Fatal("default state root still uses the shared temp directory")
	}
}

func TestCreateConflictsWithoutOverwritingJournal(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	opts := memoryOpts(t, ws, state, "sess-once")
	s, err := sdk.CreateAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)
	before, err := os.ReadFile(journalPath(state, "sess-once"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = sdk.CreateAgentSession(context.Background(), opts)
	expectCode(t, err, sdk.CodeStateConflict)
	after, err := os.ReadFile(journalPath(state, "sess-once"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("create overwrote an existing journal")
	}
}

func TestStartErrorReleasesStore(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	opts := memoryOpts(t, ws, state, "sess-start-err")
	opts.Profile = sdk.ProfileDefault
	_, err := sdk.CreateAgentSession(context.Background(), opts)
	if err == nil {
		t.Fatal("default profile must fail")
	}
	opts.Profile = sdk.ProfileMemory
	s, err := sdk.OpenAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)
}

func TestStoreOpenErrorDropsManifest(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	dir := filepath.Join(state, "sessions", "sess-locked")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(dir, "writer.lock"))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer lock.Unlock()
	_, err = sdk.CreateAgentSession(context.Background(), memoryOpts(t, ws, state, "sess-locked"))
	if err == nil {
		t.Fatal("locked store was opened")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "resources", "*", "manifest.json"))
	if len(matches) != 0 {
		t.Fatalf("store open failure left manifests: %v", matches)
	}
}

func TestOpenDoesNotCreateMissingSession(t *testing.T) {
	state := t.TempDir()
	_, err := sdk.OpenAgentSession(context.Background(), memoryOpts(t, t.TempDir(), state, "sess-missing"))
	expectCode(t, err, sdk.CodeNotFound)
	if _, err := os.Stat(filepath.Join(state, "sessions", "sess-missing")); !os.IsNotExist(err) {
		t.Fatalf("open created a missing session: %v", err)
	}
}

func TestOpenRestoresWorkspaceAndRejectsSwitch(t *testing.T) {
	ws := t.TempDir()
	other := t.TempDir()
	state := t.TempDir()
	opts := memoryOpts(t, ws, state, "sess-bind")
	s, err := sdk.CreateAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)
	opts.Workspace = other
	_, err = sdk.OpenAgentSession(context.Background(), opts)
	expectCode(t, err, sdk.CodeStateConflict)
	opts.Workspace = ""
	reopened, err := sdk.OpenAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, reopened)
	bound := hostRealRoot(t, state, "sess-bind")
	realWS, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(bound, realWS) {
		t.Fatalf("reopen binding %q, want %q", bound, realWS)
	}
}

func TestNilModelRejectedWhenExecuting(t *testing.T) {
	opts := memoryOpts(t, t.TempDir(), t.TempDir(), "sess-no-model")
	opts.Model = nil
	_, err := sdk.CreateAgentSession(context.Background(), opts)
	expectCode(t, err, sdk.CodeInvalidArgument)
}

func TestMatchingToolInfoAccepted(t *testing.T) {
	opts := memoryOpts(t, t.TempDir(), t.TempDir(), "sess-info-ok")
	opts.Tools = []sdk.ToolDefinition{{
		Name: "add", Version: "v1", Description: "add a number",
		Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
		Run:    func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	}}
	opts.ToolInfos = []*schema.ToolInfo{testkit.ToolInfo("add", "add a number")}
	s, err := sdk.CreateAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)
}

func TestToolInfoMustMatchDescription(t *testing.T) {
	opts := memoryOpts(t, t.TempDir(), t.TempDir(), "sess-desc")
	opts.Tools = []sdk.ToolDefinition{{
		Name: "add", Version: "v1", Description: "different explanation",
		Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
		Run:    func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	}}
	opts.ToolInfos = []*schema.ToolInfo{testkit.ToolInfo("add", "add a number")}
	_, err := sdk.CreateAgentSession(context.Background(), opts)
	expectCode(t, err, sdk.CodeInvalidArgument)
}

func TestOpenRejectsOversizedHeader(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	opts := memoryOpts(t, ws, state, "sess-long-header")
	s, err := sdk.CreateAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)
	path := journalPath(state, "sess-long-header")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pad := bytes.Repeat([]byte(" "), 128)
	if err := os.WriteFile(path, append(pad, raw...), 0o600); err != nil {
		t.Fatal(err)
	}
	opts.Limits.MaxCommitLineBytes = 64
	_, err = sdk.OpenAgentSession(context.Background(), opts)
	expectCode(t, err, sdk.CodeInvalidArgument)
}

func TestToolInfoMustMatchDefinitionSchema(t *testing.T) {
	opts := memoryOpts(t, t.TempDir(), t.TempDir(), "sess-schema")
	opts.Tools = []sdk.ToolDefinition{{
		Name: "add", Version: "v1", Description: "add a number",
		Schema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
		Run:    func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	}}
	opts.ToolInfos = []*schema.ToolInfo{testkit.ToolInfo("add", "add a number")}
	_, err := sdk.CreateAgentSession(context.Background(), opts)
	expectCode(t, err, sdk.CodeInvalidArgument)
}

func TestOpenRejectsSwappedToolDeclaration(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	run := func(context.Context, json.RawMessage) (string, error) { return "old", nil }
	opts := memoryOpts(t, ws, state, "sess-tools")
	opts.Tools = []sdk.ToolDefinition{{
		Name: "add", Version: "v1", Description: "adds",
		Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
		Run:    run,
	}}
	s, err := sdk.CreateAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := s.SubmitInput(ctx, sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)}); err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)

	opts.Tools[0].Description = "replaced"
	opts.Tools[0].Run = func(context.Context, json.RawMessage) (string, error) { return "new", nil }
	_, err = sdk.OpenAgentSession(context.Background(), opts)
	expectCode(t, err, sdk.CodeIncompatibleVersion)

	opts.Tools[0].Description = "adds"
	opts.Tools[0].Run = func(context.Context, json.RawMessage) (string, error) { return "other-func", nil }
	reopened, err := sdk.OpenAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, reopened)
}

func TestNoToolsUseBuiltinGeneration(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	s, err := sdk.CreateAgentSession(context.Background(), memoryOpts(t, ws, state, "sess-builtin"))
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)
	path := filepath.Join(state, "sessions", "sess-builtin", "resources", "builtin-v1", "manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("manifest is not json: %s", raw)
	}
}

func TestReadOnlyBrowsesWithoutManifestModelOrWorkspace(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	opts := memoryOpts(t, ws, state, "sess-ro")
	s, err := sdk.CreateAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)
	if err := os.RemoveAll(filepath.Join(state, "sessions", "sess-ro", "resources")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(ws); err != nil {
		t.Fatal(err)
	}
	opts.Model = nil
	opts.Workspace = ws
	_, err = sdk.OpenAgentSession(context.Background(), opts)
	expectCode(t, err, sdk.CodeInvalidArgument)
	opts.ReadOnly = true
	reopened, err := sdk.OpenAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	closeSession(t, reopened)
}

func TestEmptyToolVersionRejected(t *testing.T) {
	opts := memoryOpts(t, t.TempDir(), t.TempDir(), "sess-no-ver")
	opts.Tools = []sdk.ToolDefinition{{
		Name:   "add",
		Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
		Run:    func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	}}
	_, err := sdk.CreateAgentSession(context.Background(), opts)
	expectCode(t, err, sdk.CodeInvalidArgument)
}

func memoryOpts(t *testing.T, workspace, state, id string) sdk.SessionOptions {
	t.Helper()
	return sdk.SessionOptions{
		Workspace: workspace, StateRoot: state, SessionID: id,
		Profile: sdk.ProfileMemory, Model: testkit.NewFake(testkit.Step{Text: "done"}),
		Instruction: "memory tools only",
	}
}

func closeSession(t *testing.T, s interface{ Close(context.Context) error }) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func journalPath(state, id string) string {
	return filepath.Join(state, "sessions", id, "journal.jsonl")
}

func hostRealRoot(t *testing.T, state, id string) string {
	t.Helper()
	f, err := os.Open(journalPath(state, id))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var header struct {
		Workspace json.RawMessage `json:"workspace"`
	}
	if err := json.NewDecoder(f).Decode(&header); err != nil {
		t.Fatal(err)
	}
	var binding struct {
		HostRealRoot string `json:"hostRealRoot"`
	}
	if err := json.Unmarshal(header.Workspace, &binding); err != nil {
		t.Fatal(err)
	}
	return binding.HostRealRoot
}

func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	pe, ok := sdk.AsError(err)
	if !ok || pe.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return filepath.Clean(a) == filepath.Clean(b) || equalFold(a, b)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func equalFold(a, b string) bool {
	return len(a) == len(b) && filepath.Clean(a) != "" && (a == b || filepath.Clean(a) == filepath.Clean(b) || osAgnosticEqual(a, b))
}

func osAgnosticEqual(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return stringsEqualFold(aa, bb)
	}
	return aa == bb
}

func stringsEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
