package workflowagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

// The installed operations capability probe is an existing public factory
// boundary after compatibility inspection and before the final backend open.
// It never starts a process, model or tool.
type workflowFactoryProbe struct {
	agent.ProcessOperations
	before func()
	calls  int
}

func (p *workflowFactoryProbe) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	p.calls++
	if p.before != nil {
		p.before()
	}
	return agent.BackendCapabilities{BackendID: "factory-probe", Version: "v1", EnvironmentID: "offline", SupportedModes: []string{"workspace-write"}, Enforcement: "full", RuntimeDataWriteProtected: true}, nil
}

func workflowFactoryPair(t *testing.T) (WorkflowOptions, WorkflowOptions, *testkit.FakeModel, *atomic.Int32) {
	t.Helper()
	count := new(atomic.Int32)
	fake := testkit.NewFake()
	opts := testOptions(t, modelThenTool(), fake, count)
	w := newWorkflow(t, opts)
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	other := opts
	other.StateRoot = t.TempDir()
	other.Principal = "other"
	other.GenerationFingerprint = "other-v2"
	other.Definition.Version = "v2"
	foreign := newWorkflow(t, other)
	if err := foreign.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Both are ordinary, independently valid runs with the same resource ID.
	ro := other
	ro.ReadOnly = true
	valid, err := OpenWorkflowAgent(t.Context(), ro)
	if err != nil {
		t.Fatal(err)
	}
	if err := valid.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	return opts, other, fake, count
}

func workflowFactoryJournal(opts WorkflowOptions) string {
	return filepath.Join(opts.StateRoot, "workflow-runs", opts.RunID, "journal.jsonl")
}

func workflowFactoryEncode(t *testing.T, loaded storage.StoredSession) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(loaded.Header); err != nil {
		t.Fatal(err)
	}
	for _, commit := range loaded.Commits {
		if err := enc.Encode(commit); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestWorkflowFactoryRootsLatestRetainsImmutableInspection(t *testing.T) {
	for _, kind := range []string{"principal", "manifest", "workspace", "policy", "limits", "header"} {
		t.Run(kind, func(t *testing.T) {
			var effects atomic.Int32
			fake := testkit.NewFake()
			opts := testOptions(t, modelThenTool(), fake, &effects)
			w := newWorkflow(t, opts)
			if err := w.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			name := workflowFactoryJournal(opts)
			info, err := os.Stat(name)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := jsonl.ReadFile(bytes.NewReader(workflowFactoryBytes(t, name)), opts.RunID, 0)
			if err != nil {
				t.Fatal(err)
			}
			var init initialRecord
			var wb workspaceBinding
			if json.Unmarshal(loaded.Commits[0].ControlRecords[0].Payload, &init) != nil || json.Unmarshal(loaded.Header.Workspace, &wb) != nil {
				t.Fatal("invalid original fixture")
			}
			code := product.CodeIncompatibleVersion
			switch kind {
			case "principal":
				init.Principal = "other"
				code = product.CodePermissionDenied
			case "manifest":
				init.Manifest.Fingerprint = "other-v2"
				init.BindingVersion = digest(init.Manifest)
				wb.BindingVersion = init.BindingVersion
			case "workspace":
				init.Workspace = t.TempDir()
				wb.HostRealRoot = init.Workspace
			case "policy":
				init.Policy, err = normalizePolicy(&agent.ResolvedPolicy{SandboxMode: "read-only"})
				if err != nil {
					t.Fatal(err)
				}
			case "limits":
				init.Limits.ActivityBudget++
			case "header":
				loaded.Header.CreatedAt = loaded.Header.CreatedAt.Add(1)
			}
			loaded.Commits[0].ControlRecords[0] = record("workflow_initialized", "initial", init)
			loaded.Header.Workspace, _ = json.Marshal(wb)
			changed := workflowFactoryEncode(t, loaded)
			// This is a valid same-inode replacement, not a corrupt replay fixture.
			valid, err := jsonl.ReadFile(bytes.NewReader(changed), opts.RunID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := replay(valid, opts.RunID); err != nil {
				t.Fatal(err)
			}
			probe := &workflowFactoryProbe{before: func() {
				if err := os.WriteFile(name, changed, 0600); err != nil {
					t.Fatal(err)
				}
				current, err := os.Stat(name)
				if err != nil || !os.SameFile(info, current) {
					t.Fatal("mutation did not retain actual file identity")
				}
			}}
			opts.Operations.Process = probe
			opened, err := OpenWorkflowAgent(t.Context(), opts)
			if opened != nil {
				opened.Close(context.Background())
				t.Errorf("latest replay bypassed prechecked immutable %s", kind)
			}
			requireCode(t, err, code)
			if probe.calls != 1 || fake.Calls() != 0 || effects.Load() != 0 {
				t.Fatal("immutable inspection check executed a model or tool")
			}
			// Failed owned open must release its writer lock for the actual run.
			roots, err := storage.OpenResourceRoots(opts.StateRoot, storage.ResourceWorkflow, opts.RunID, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			backend, err := jsonl.OpenBound(opts.RunID, roots, nil, storage.Header{}, jsonl.Options{ResourceType: storage.ResourceWorkflow, OpenExisting: true})
			if err != nil {
				roots.Close()
				t.Fatal(err)
			}
			if err := backend.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func workflowFactoryBytes(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type workflowFactoryFileState struct {
	Mode os.FileMode
	Size int64
	Raw  string
}

func workflowFactoryTree(t *testing.T, dir string) map[string]workflowFactoryFileState {
	t.Helper()
	out := map[string]workflowFactoryFileState{}
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		state := workflowFactoryFileState{Mode: info.Mode()}
		if info.Mode().IsRegular() {
			state.Size = info.Size()
			state.Raw = string(workflowFactoryBytes(t, path))
		}
		out[name] = state
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func workflowFactoryCheckClosed(t *testing.T, roots *storage.ResourceRoots, inspected *os.File) {
	t.Helper()
	if inspected != nil {
		// Windows File.Stat reports ERROR_INVALID_HANDLE after Close rather
		// than os.ErrClosed. A failed Stat plus an actual second Close proves
		// the file is closed without weakening the platform-specific result.
		if _, err := inspected.Stat(); err == nil {
			t.Fatal("factory did not close its inspection file")
		}
		if err := inspected.Close(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("inspection Close was not already completed: %v", err)
		}
	}
	if roots != nil {
		for _, root := range []*os.Root{roots.Resource, roots.Namespace} {
			if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("owned root remained open: %v", err)
			}
		}
	}
}

func TestWorkflowFactoryRootsBoundOpenOwnershipAndReadOnly(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		name := "writable"
		if readOnly {
			name = "readonly"
		}
		t.Run(name, func(t *testing.T) {
			var effects atomic.Int32
			fake := testkit.NewFake()
			opts := testOptions(t, modelThenTool(), fake, &effects)
			w := newWorkflow(t, opts)
			if err := w.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			journal := workflowFactoryJournal(opts)
			if err := os.Remove(filepath.Join(filepath.Dir(journal), "writer.lock")); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{filepath.Dir(filepath.Dir(journal)), filepath.Dir(journal)} {
				if err := os.Chmod(path, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(journal, 0644); err != nil {
				t.Fatal(err)
			}
			before := workflowFactoryTree(t, opts.StateRoot)
			opts.ReadOnly = readOnly
			probe := &workflowFactoryProbe{}
			opts.Operations.Process = probe
			if readOnly {
				opts.Models, opts.Tools = nil, nil
				opts.Definition = WorkflowDefinition{}
			}
			children, finals := 0, 0
			var retained *storage.ResourceRoots
			var inspection *os.File
			var bound *jsonl.Store
			opened, err := openWorkflowAgent(t.Context(), opts, func(parent *os.Root, name string) (*os.Root, error) {
				children++
				return parent.OpenRoot(name)
			}, func(id string, roots *storage.ResourceRoots, inspected *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
				finals++
				retained, inspection = roots, inspected
				if children != 2 || inspected == nil || opt.ReadOnly != readOnly || !opt.OpenExisting || opt.ResourceType != storage.ResourceWorkflow || opt.MaxLine != opts.Limits.WithDefaults().MaxCommitLineBytes {
					t.Fatal("final bound store was not called with the single inspected resource")
				}
				position, err := inspected.Seek(0, io.SeekCurrent)
				if err != nil || position != 0 {
					t.Fatal("prefix inspection changed the owned file offset")
				}
				info, err := inspected.Stat()
				if err != nil {
					t.Fatal(err)
				}
				bound, err = jsonl.OpenBound(id, roots, inspected, header, opt)
				if err != nil {
					return nil, err
				}
				t.Cleanup(func() { _ = bound.Close() })
				finalInfo, err := bound.ResourceRoot().Stat("journal.jsonl")
				if err != nil || bound.ResourceRoot() != roots.Resource || !os.SameFile(info, finalInfo) {
					t.Fatal("actual bound store lost the inspected Root/File identity")
				}
				return bound, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { opened.Close(context.Background()) })
			if children != 2 || finals != 1 || fake.Calls() != 0 || effects.Load() != 0 {
				t.Fatal("factory reopened a namespace/resource or executed a model/tool")
			}
			workflowFactoryCheckClosed(t, nil, inspection)
			if readOnly {
				if probe.calls != 0 || !reflect.DeepEqual(before, workflowFactoryTree(t, opts.StateRoot)) {
					t.Fatal("read-only open created, chmodded, locked, changed bytes or probed operations")
				}
			} else if probe.calls != 1 {
				t.Fatal("writable capability probe count changed")
			}
			snapshot, err := opened.Snapshot(t.Context())
			if err != nil || snapshot.State != "created" || snapshot.Revision != 1 {
				t.Fatalf("bound factory replay changed state: %v", err)
			}
			if err := opened.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := opened.Close(t.Context()); err != nil {
				t.Fatal("repeated close lost idempotence")
			}
			workflowFactoryCheckClosed(t, retained, inspection)
			if bound.ResourceRoot() != nil {
				t.Fatal("store retained owned roots after Close")
			}
			if readOnly && !reflect.DeepEqual(before, workflowFactoryTree(t, opts.StateRoot)) {
				t.Fatal("read-only Close changed the resource tree")
			}
		})
	}
}

func TestWorkflowFactoryRootsPreflightRejectsBeforeFinalBackend(t *testing.T) {
	for _, kind := range []string{"principal", "readonly_principal", "definition", "model", "tool", "generation", "policy", "operations", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			var effects atomic.Int32
			fake := testkit.NewFake()
			opts := testOptions(t, modelThenTool(), configuredFake{AgenticModel: fake, version: "model-v1"}, &effects)
			w := newWorkflow(t, opts)
			if err := w.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			journal := workflowFactoryJournal(opts)
			if err := os.Remove(filepath.Join(filepath.Dir(journal), "writer.lock")); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{filepath.Dir(filepath.Dir(journal)), filepath.Dir(journal)} {
				if err := os.Chmod(path, 0755); err != nil {
					t.Fatal(err)
				}
			}
			before := workflowFactoryTree(t, opts.StateRoot)
			code := product.CodeIncompatibleVersion
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch kind {
			case "principal", "readonly_principal":
				opts.Principal = "other"
				opts.ReadOnly = kind == "readonly_principal"
				code = product.CodePermissionDenied
			case "definition":
				opts.Definition.Version = "v2"
			case "model":
				opts.Models["chosen"] = configuredFake{AgenticModel: fake, version: "model-v2"}
			case "tool":
				opts.Tools[0].Version = "v2"
			case "generation":
				opts.GenerationFingerprint = "other-v2"
			case "policy":
				opts.Policy = &agent.ResolvedPolicy{SandboxMode: "read-only"}
				code = product.CodeStateConflict
			case "operations":
				opts.Operations.Process = &workflowFactoryProbeInvalid{}
				code = product.CodeResourceUnavailable
			case "cancel":
				opts.Operations.Process = &workflowFactoryProbe{before: cancel}
			}
			var held []*os.Root
			children, finals := 0, 0
			opened, err := openWorkflowAgent(ctx, opts, func(parent *os.Root, name string) (*os.Root, error) {
				children++
				root, err := parent.OpenRoot(name)
				if err == nil {
					held = append(held, root)
				}
				return root, err
			}, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
				finals++
				return jsonl.OpenBound(id, roots, file, header, opt)
			})
			if opened != nil {
				opened.Close(context.Background())
				t.Fatal("preflight rejection opened an agent")
			}
			if kind == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost identity: %v", err)
				}
			} else {
				requireCode(t, err, code)
			}
			if children != 2 || finals != 0 || fake.Calls() != 0 || effects.Load() != 0 || !reflect.DeepEqual(before, workflowFactoryTree(t, opts.StateRoot)) {
				t.Fatal("rejected preflight opened a final backend, changed modes/tree/journal, or executed")
			}
			for _, root := range held {
				if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
					t.Fatal("rejected preflight leaked an owned root")
				}
			}
			// On Windows this also proves the reader's no-DELETE handle was
			// released after failure rather than only the directory handles.
			if err := os.Rename(journal, journal+".closed"); err != nil {
				t.Fatal("inspection handle survived the rejected open")
			}
			if err := os.Rename(journal+".closed", journal); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type workflowFactoryProbeInvalid struct{ agent.ProcessOperations }

func (*workflowFactoryProbeInvalid) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return agent.BackendCapabilities{}, nil
}

func TestWorkflowFactoryRootsClosedBindingsNeverFallback(t *testing.T) {
	for _, kind := range []string{"roots", "inspection", "inspection_close_after_success"} {
		t.Run(kind, func(t *testing.T) {
			var effects atomic.Int32
			opts := testOptions(t, toolOnly(), nil, &effects)
			w := newWorkflow(t, opts)
			if err := w.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			journal := workflowFactoryJournal(opts)
			if err := os.Remove(filepath.Join(filepath.Dir(journal), "writer.lock")); err != nil {
				t.Fatal(err)
			}
			before := workflowFactoryTree(t, opts.StateRoot)
			var retained *storage.ResourceRoots
			var inspection *os.File
			var bound *jsonl.Store
			finals := 0
			opened, err := openWorkflowAgent(t.Context(), opts, nil, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
				finals++
				retained, inspection = roots, file
				if kind == "roots" {
					if err := roots.Close(); err != nil {
						t.Fatal(err)
					}
				} else if kind == "inspection" {
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				bound, err = jsonl.OpenBound(id, roots, file, header, opt)
				if err == nil && kind == "inspection_close_after_success" {
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
				}
				return bound, err
			})
			if opened != nil {
				opened.Close(context.Background())
				t.Fatal("closed binding used a path fallback")
			}
			requireCode(t, err, product.CodeStorageUnavailable)
			if finals != 1 || effects.Load() != 0 {
				t.Fatal("closed-binding factory invocation count changed")
			}
			workflowFactoryCheckClosed(t, retained, inspection)
			if kind != "inspection_close_after_success" && !reflect.DeepEqual(before, workflowFactoryTree(t, opts.StateRoot)) {
				t.Fatal("closed pre-writer binding created or changed a journal/lock")
			}
			if bound != nil && bound.ResourceRoot() != nil {
				t.Fatal("inspection close failure did not close the new owned store")
			}
			// The failure must leave no writer ownership behind.
			valid, err := OpenWorkflowAgent(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := valid.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWorkflowFactoryRootsInvalidTypeAndIdentityNeverFallback(t *testing.T) {
	for _, kind := range []string{"journal_directory", "resource_type", "run_identity", "closed_relative_root"} {
		t.Run(kind, func(t *testing.T) {
			var effects atomic.Int32
			fake := testkit.NewFake()
			opts := testOptions(t, modelThenTool(), fake, &effects)
			w := newWorkflow(t, opts)
			if err := w.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			journal := workflowFactoryJournal(opts)
			code := product.CodeInvalidArgument
			switch kind {
			case "journal_directory":
				if err := os.Remove(journal); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(journal, 0755); err != nil {
					t.Fatal(err)
				}
			case "resource_type", "run_identity":
				loaded, err := jsonl.ReadFile(bytes.NewReader(workflowFactoryBytes(t, journal)), opts.RunID, 0)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "resource_type" {
					loaded.Header.ResourceType = storage.ResourceCode
					loaded.Header.SessionID, loaded.Header.RunID = opts.RunID, ""
					code = product.CodeIncompatibleVersion
				} else {
					loaded.Header.RunID = "other-run"
					code = product.CodeNotFound
				}
				changed := workflowFactoryEncode(t, loaded)
				if kind == "run_identity" {
					_, readErr := jsonl.ReadFile(bytes.NewReader(changed), opts.RunID, 0)
					requireCode(t, readErr, product.CodeNotFound)
				}
				if err := os.WriteFile(journal, changed, 0600); err != nil {
					t.Fatal(err)
				}
			case "closed_relative_root":
				code = product.CodeStorageUnavailable
			}
			opts.ReadOnly = true
			before := workflowFactoryTree(t, opts.StateRoot)
			finals := 0
			opened, err := openWorkflowAgent(t.Context(), opts, func(parent *os.Root, name string) (*os.Root, error) {
				root, err := parent.OpenRoot(name)
				if err == nil && kind == "closed_relative_root" && name == opts.RunID {
					if err := root.Close(); err != nil {
						t.Fatal(err)
					}
				}
				return root, err
			}, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
				finals++
				return jsonl.OpenBound(id, roots, file, header, opt)
			})
			if opened != nil {
				opened.Close(context.Background())
				t.Fatal("invalid resource type/identity used a path fallback")
			}
			requireCode(t, err, code)
			if finals != 0 || fake.Calls() != 0 || effects.Load() != 0 || !reflect.DeepEqual(before, workflowFactoryTree(t, opts.StateRoot)) {
				t.Fatal("invalid read-only binding opened a backend or changed resource state")
			}
		})
	}
}

func TestWorkflowFactoryRootsFinalBoundRejectsDifferentJournal(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "writable", true: "readonly"}[readOnly], func(t *testing.T) {
			opts, other, fake, effects := workflowFactoryPair(t)
			opts.ReadOnly = readOnly
			journal, otherJournal := workflowFactoryJournal(opts), workflowFactoryJournal(other)
			original, outside := workflowFactoryBytes(t, journal), workflowFactoryBytes(t, otherJournal)
			var retained *storage.ResourceRoots
			var inspection *os.File
			var renameErr error
			finals := 0
			opened, err := openWorkflowAgent(t.Context(), opts, nil, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
				finals++
				retained, inspection = roots, file
				renameErr = os.Rename(journal, journal+".parked")
				if renameErr == nil {
					if err := os.Rename(otherJournal, journal); err != nil {
						t.Fatal(err)
					}
				}
				return jsonl.OpenBound(id, roots, file, header, opt)
			})
			if finals != 1 || inspection == nil || fake.Calls() != 0 || effects.Load() != 0 {
				t.Fatal("actual final OpenBound boundary count/inspection changed")
			}
			if renameErr == nil {
				if opened != nil {
					opened.Close(context.Background())
					t.Fatal("actual different-file final binding was accepted")
				}
				requireCode(t, err, product.CodeInvalidArgument)
				workflowFactoryCheckClosed(t, retained, inspection)
				if !bytes.Equal(original, workflowFactoryBytes(t, journal+".parked")) || !bytes.Equal(outside, workflowFactoryBytes(t, journal)) {
					t.Fatal("different-file rejection wrote either journal")
				}
			} else {
				workflowFactorySharingDenied(t, renameErr)
				if err != nil || opened == nil {
					t.Fatalf("Windows blocked rename lost original bound run: %v", err)
				}
				defer opened.Close(context.Background())
				workflowFactoryCheckClosed(t, nil, inspection)
				if opened.state.Initial.Principal != opts.Principal || opened.binding != opened.state.Initial.BindingVersion || !bytes.Equal(original, workflowFactoryBytes(t, journal)) || !bytes.Equal(outside, workflowFactoryBytes(t, otherJournal)) {
					t.Fatal("blocked final rename changed the accepted binding or journals")
				}
			}
		})
	}
}

func TestWorkflowFactoryRootsLatestAllowsNewCommit(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "writable", true: "readonly"}[readOnly], func(t *testing.T) {
			var effects atomic.Int32
			fake := testkit.NewFake()
			opts := testOptions(t, modelThenTool(), fake, &effects)
			w := newWorkflow(t, opts)
			if err := w.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			openOpts := opts
			openOpts.ReadOnly = readOnly
			finals := 0
			var inspection *os.File
			opened, err := openWorkflowAgent(t.Context(), openOpts, nil, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
				finals++
				inspection = file
				writer, err := OpenWorkflowAgent(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := writer.Cancel(t.Context(), WorkflowControlCommand{Principal: opts.Principal}); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				return jsonl.OpenBound(id, roots, file, header, opt)
			})
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close(context.Background())
			workflowFactoryCheckClosed(t, nil, inspection)
			snapshot, err := opened.Snapshot(t.Context())
			if err != nil || snapshot.State != "cancelled" || snapshot.Revision != 2 || finals != 1 || fake.Calls() != 0 || effects.Load() != 0 || opened.binding != opened.state.Initial.BindingVersion {
				t.Fatalf("legitimate commit was not replayed without execution: %v", err)
			}
		})
	}
}

func TestWorkflowFactoryRootsCreateRetainsSingleRoot(t *testing.T) {
	opts, other, fake, effects := workflowFactoryPair(t)
	// Use a fresh absent run beneath the original state root for owned Create.
	// The replacement is a separately valid run with that same ID.
	opts.StateRoot = t.TempDir()
	otherJournal := workflowFactoryJournal(other)
	outside := workflowFactoryBytes(t, otherJournal)
	children, finals := 0, 0
	var retained *storage.ResourceRoots
	var renameErr error
	var parked string
	created, err := createWorkflowAgent(t.Context(), opts, func(parent *os.Root, name string) (*os.Root, error) {
		children++
		return parent.OpenRoot(name)
	}, func(id string, roots *storage.ResourceRoots, inspected *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
		finals++
		retained = roots
		if children != 2 || inspected != nil || opt.ReadOnly || opt.OpenExisting || opt.ResourceType != storage.ResourceWorkflow {
			t.Fatal("create did not use its sole new resource root")
		}
		parked = roots.Path + ".parked"
		renameErr = os.Rename(roots.Path, parked)
		if renameErr == nil {
			if err := os.Rename(filepath.Dir(otherJournal), roots.Path); err != nil {
				t.Fatal(err)
			}
		}
		return jsonl.OpenBound(id, roots, inspected, header, opt)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer created.Close(context.Background())
	if children != 2 || finals != 1 || fake.Calls() != 0 || effects.Load() != 0 || created.state.Revision != 1 || created.state.Initial.Principal != opts.Principal {
		t.Fatal("create reopened a resource, repeated initialization or exposed foreign state")
	}
	backend := created.store.(*jsonl.Store)
	if backend.ResourceRoot() != retained.Resource {
		t.Fatal("create backend does not own its original root")
	}
	originalPath, foreignPath := workflowFactoryJournal(opts), otherJournal
	if renameErr == nil {
		originalPath = filepath.Join(parked, "journal.jsonl")
		foreignPath = workflowFactoryJournal(opts)
	} else {
		workflowFactorySharingDenied(t, renameErr)
	}
	if !bytes.Equal(outside, workflowFactoryBytes(t, foreignPath)) {
		t.Fatal("create changed the exchanged foreign journal")
	}
	loaded, err := jsonl.ReadFile(bytes.NewReader(workflowFactoryBytes(t, originalPath)), opts.RunID, 0)
	if err != nil || len(loaded.Commits) != 1 || len(loaded.Commits[0].ControlRecords) != 1 || loaded.Commits[0].ControlRecords[0].Type != "workflow_initialized" {
		t.Fatalf("create changed its single initialization commit order: %v", err)
	}
	if err := created.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	workflowFactoryCheckClosed(t, retained, nil)
}

func TestWorkflowFactoryRootsCreateExistingJournalNeverOpensBackend(t *testing.T) {
	for _, kind := range []string{"directory", "empty", "ordinary"} {
		t.Run(kind, func(t *testing.T) {
			var effects atomic.Int32
			opts := testOptions(t, toolOnly(), nil, &effects)
			roots, err := storage.OpenResourceRoots(opts.StateRoot, storage.ResourceWorkflow, opts.RunID, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			name := workflowFactoryJournal(opts)
			if err := roots.Close(); err != nil {
				t.Fatal(err)
			}
			if kind == "directory" {
				err = os.Mkdir(name, 0755)
			} else {
				raw := []byte(nil)
				if kind == "ordinary" {
					raw = []byte("an existing incompatible journal\n")
				}
				err = os.WriteFile(name, raw, 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := workflowFactoryTree(t, opts.StateRoot)
			finals := 0
			created, err := createWorkflowAgent(t.Context(), opts, nil, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
				finals++
				return jsonl.OpenBound(id, roots, file, header, opt)
			})
			if created != nil {
				created.Close(context.Background())
				t.Fatal("create admitted an existing journal")
			}
			requireCode(t, err, product.CodeStateConflict)
			if finals != 0 || effects.Load() != 0 || !reflect.DeepEqual(before, workflowFactoryTree(t, opts.StateRoot)) {
				t.Fatal("existing journal caused writer/chmod or execution side effects")
			}
		})
	}
}

type workflowFactoryStoreProbe struct {
	storage.Store
	loads, closes int
	loadErr       error
	closeErr      error
}

func (s *workflowFactoryStoreProbe) Load(ctx context.Context, id string) (storage.StoredSession, error) {
	s.loads++
	if s.loadErr != nil {
		return storage.StoredSession{}, s.loadErr
	}
	return s.Store.Load(ctx, id)
}

func (s *workflowFactoryStoreProbe) Close() error {
	s.closes++
	return errors.Join(s.Store.Close(), s.closeErr)
}

func TestWorkflowFactoryRootsInjectedOwnershipIsUnchanged(t *testing.T) {
	var effects atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &effects)
	injected := injectStore(t, &opts)
	probe := &workflowFactoryStoreProbe{Store: injected}
	opts.Store = probe
	children, finals := 0, 0
	child := func(parent *os.Root, name string) (*os.Root, error) {
		children++
		return parent.OpenRoot(name)
	}
	final := func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
		finals++
		return jsonl.OpenBound(id, roots, file, header, opt)
	}
	created, err := createWorkflowAgent(t.Context(), opts, child, final)
	if err != nil {
		t.Fatal(err)
	}
	if probe.loads != 1 || probe.closes != 0 || children != 0 || finals != 0 || created.state.Revision != 1 {
		t.Fatal("injected create acquired roots or changed its Load/ownership contract")
	}
	if err := created.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	probe.closes = 0
	_, err = createWorkflowAgent(t.Context(), opts, child, final)
	requireCode(t, err, product.CodeStateConflict)
	bad := opts
	bad.Principal = "other"
	_, err = openWorkflowAgent(t.Context(), bad, child, final)
	requireCode(t, err, product.CodePermissionDenied)
	primary := product.NewError(product.CodeIncompatibleVersion, "injected workflow compatibility failure")
	probe.loadErr = errors.Join(primary, &os.PathError{Op: "read", Path: "private-location", Err: os.ErrClosed})
	_, err = openWorkflowAgent(t.Context(), opts, child, final)
	if err != primary {
		t.Fatal("injected compatibility error is not directly code-identifiable")
	}
	if probe.closes != 0 || children != 0 || finals != 0 || effects.Load() != 0 || probe.loads != 4 {
		t.Fatal("failed injected factory closed its caller-owned store or acquired filesystem resources")
	}
	probe.loadErr = nil
	opened, err := openWorkflowAgent(t.Context(), opts, child, final)
	if err != nil {
		t.Fatal(err)
	}
	if probe.loads != 5 || probe.closes != 0 || opened.state.Revision != 1 {
		t.Fatal("injected open performed an extra latest Load")
	}
	if err := opened.Close(t.Context()); err != nil || probe.closes != 1 {
		t.Fatal("successful injected workflow Close ownership changed")
	}
}

func TestWorkflowFactoryRootsCleanupPreservesPrimaryCode(t *testing.T) {
	for _, kind := range []string{"compatibility", "compatibility_cleanup_cancel", "cancel", "deadline", "raw_close"} {
		t.Run(kind, func(t *testing.T) {
			var effects atomic.Int32
			opts := testOptions(t, toolOnly(), nil, &effects)
			w := newWorkflow(t, opts)
			if err := w.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			roots, err := storage.OpenResourceRoots(opts.StateRoot, storage.ResourceWorkflow, opts.RunID, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			file, err := jsonl.OpenJournalReader(roots.Resource)
			if err != nil {
				t.Fatal(err)
			}
			backend, err := jsonl.OpenBound(opts.RunID, roots, file, storage.Header{}, jsonl.Options{ResourceType: storage.ResourceWorkflow, OpenExisting: true})
			if err != nil {
				t.Fatal(err)
			}
			probe := &workflowFactoryStoreProbe{Store: backend, closeErr: &os.PathError{Op: "close", Path: "private-location", Err: os.ErrClosed}}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			var cause error
			switch kind {
			case "compatibility", "compatibility_cleanup_cancel":
				cause = product.NewError(product.CodeIncompatibleVersion, "workflow compatibility failure")
				if kind == "compatibility_cleanup_cancel" {
					probe.closeErr = context.Canceled
				}
			case "cancel":
				cause = context.Canceled
			case "deadline":
				cause = context.DeadlineExceeded
			}
			err = closeWorkflowFactoryOwned(cause, probe, file, roots)
			if cause != nil {
				if err != cause {
					t.Fatalf("cleanup displaced the primary code/context error: %v", err)
				}
			} else {
				requireCode(t, err, product.CodeStorageUnavailable)
				if err.Error() != "storage_unavailable: workflow store operation failed" {
					t.Fatal("raw cleanup error exposed an OS path")
				}
			}
			workflowFactoryCheckClosed(t, roots, file)
			if probe.closes != 1 || backend.ResourceRoot() != nil || effects.Load() != 0 {
				t.Fatal("cleanup stopped after an earlier failure")
			}
			if err := backend.Close(); err != nil {
				t.Fatal("owned backend repeat Close lost idempotence")
			}
		})
	}
}

func workflowFactorySharingDenied(t *testing.T, err error) {
	t.Helper()
	var errno syscall.Errno
	if runtime.GOOS != "windows" || !errors.As(err, &errno) || errno != syscall.Errno(5) && errno != syscall.Errno(32) {
		t.Fatalf("expected actual Windows access/sharing denial, got %v", err)
	}
}

func TestWorkflowFactoryRootsPublicOpenRetainsPrecheckedRun(t *testing.T) {
	for _, leaf := range []bool{true, false} {
		name := "directory"
		if leaf {
			name = "journal"
		}
		t.Run(name, func(t *testing.T) {
			opts, other, fake, count := workflowFactoryPair(t)
			original := workflowFactoryJournal(opts)
			replacement := workflowFactoryJournal(other)
			before := workflowFactoryBytes(t, original)
			outside := workflowFactoryBytes(t, replacement)
			if !leaf {
				original = filepath.Dir(original)
				replacement = filepath.Dir(replacement)
			}
			parked := original + ".parked"
			var renameErr error
			probe := &workflowFactoryProbe{before: func() {
				renameErr = os.Rename(original, parked)
				if renameErr == nil {
					if err := os.Rename(replacement, original); err != nil {
						t.Fatal(err)
					}
				}
			}}
			opts.Operations.Process = probe
			opened, err := OpenWorkflowAgent(t.Context(), opts)
			if opened != nil {
				defer opened.Close(context.Background())
				if opened.state.Initial.Principal != opts.Principal || opened.state.Initial.Manifest.Definition.Version != opts.Definition.Version || opened.binding != opened.state.Initial.BindingVersion {
					t.Fatalf("prechecked run was replaced: principal=%s definition=%s bindingMatches=%v, probe=%d model=%d tool=%d", opened.state.Initial.Principal, opened.state.Initial.Manifest.Definition.Version, opened.binding == opened.state.Initial.BindingVersion, probe.calls, fake.Calls(), count.Load())
				}
			}
			if probe.calls != 1 || fake.Calls() != 0 || count.Load() != 0 {
				t.Fatal("factory boundary executed or probe count changed")
			}
			if renameErr != nil {
				workflowFactorySharingDenied(t, renameErr)
				if err != nil || opened == nil || !bytes.Equal(before, workflowFactoryBytes(t, workflowFactoryJournal(opts))) || !bytes.Equal(outside, workflowFactoryBytes(t, workflowFactoryJournal(other))) {
					t.Fatalf("blocked rename changed original binding or journals: %v", err)
				}
				t.Log("actual Windows rename denied; original binding retained")
				return
			}
			if leaf {
				requireCode(t, err, product.CodeInvalidArgument)
				if opened != nil {
					t.Fatal("different journal identity was accepted")
				}
			} else if err != nil || opened == nil {
				t.Fatalf("retained directory binding failed: %v", err)
			}
			originalJournal, foreignJournal := parked, original
			if !leaf {
				originalJournal = filepath.Join(parked, "journal.jsonl")
				foreignJournal = filepath.Join(original, "journal.jsonl")
			}
			if !bytes.Equal(before, workflowFactoryBytes(t, originalJournal)) || !bytes.Equal(outside, workflowFactoryBytes(t, foreignJournal)) {
				t.Fatal("directory/leaf exchange changed journal bytes")
			}
			t.Log("actual directory/leaf exchange retained or rejected the original binding; both journal bytes unchanged")
		})
	}
}
