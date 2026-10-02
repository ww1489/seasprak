package codeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestFileBoundaryFactoryRuntimeManifestKeepsDefaultBinding(t *testing.T) {
	for _, phase := range []string{"Create", "Open", "ReadOnlyOpen"} {
		t.Run(phase, func(t *testing.T) {
			model := testkit.NewFake()
			var effects atomic.Int32
			opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "factory-bound", Profile: ProfileMemory, Model: model,
				GenerationFingerprint: "factory-original", Tools: []tools.Definition{countedTool("probe", &effects, nil)}}
			session, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if phase != "Create" {
				if err := session.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				opts.ReadOnly = phase == "ReadOnlyOpen"
				session, err = OpenAgentSession(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { _ = session.Close(context.Background()) })
			outside := t.TempDir()
			foreign, err := buildManifest(session.rt.generation, nil, "factory-foreign")
			if err != nil {
				t.Fatal(err)
			}
			if err := saveManifest(outside, opts.SessionID, foreign); err != nil {
				t.Fatal(err)
			}
			before := fileBoundaryTree(t, outside)
			view := session.rt.manager.View()
			if err := session.rt.do(t.Context(), func(rt *runtime) error {
				// The path is diagnostic only after the default factory transfer.
				// Do not supply a test-owned root: exercise the public factory.
				rt.opts.StateRoot = outside
				return matchGeneration(&rt.opts, rt.generation)
			}); err != nil {
				t.Fatalf("default factory runtime reopened the stale path: %v", err)
			}
			if _, err := session.Snapshot(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(view, session.rt.manager.View()) || model.Calls() != 0 || effects.Load() != 0 || len(view.Calls) != 0 {
				t.Error("generation observation committed or executed work")
			}
			if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
				t.Error("generation observation changed external bytes, modes or entries")
			}
			if root := session.rt.opts.fileRoot; root == nil {
				t.Error("successful default runtime read has no retained root")
			} else if _, err := root.Stat("."); err != nil {
				t.Fatal("runtime read closed the borrowed root:", err)
			}
		})
	}
}

func TestFileBoundaryFactorySnapshotAndResumeUseDefaultRoot(t *testing.T) {
	for _, phase := range []string{"Create", "Open"} {
		t.Run(phase, func(t *testing.T) {
			f := pausedResumeFixture(t, false, resumeFixtureOptions{Disk: true})
			session := f.s
			if phase == "Open" {
				if err := session.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				var err error
				session, err = OpenAgentSession(t.Context(), f.opts)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = session.Close(context.Background()) })
			}
			outside := t.TempDir()
			outsideBefore := fileBoundaryTree(t, outside)
			before := session.rt.manager.View()
			if err := session.rt.do(t.Context(), func(rt *runtime) error {
				rt.opts.StateRoot = outside
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			snap, err := session.Snapshot(t.Context())
			if err != nil || !snap.Resume[f.input.TraceID].CanResume {
				t.Fatalf("default root snapshot eligibility=%+v err=%v", snap.Resume[f.input.TraceID], err)
			}
			if session.rt.manager.View().LastSeq != before.LastSeq || f.model.Calls() != 1 || f.runs.Load() != 0 {
				t.Fatal("snapshot wrote or executed work")
			}
			receipt, err := session.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
			if err != nil {
				t.Fatal("default-root resume:", err)
			}
			waitResumeCondition(t, func() bool { return session.rt.manager.View().Traces[f.input.TraceID].Settled })
			tr := session.rt.manager.View().Traces[f.input.TraceID]
			if receipt.State != "accepted" || tr.State != "completed" || f.model.Calls() != 2 || f.runs.Load() != 1 || tr.Usage.LogicalModelCalls != 2 || tr.Usage.TransportRequests != 2 || tr.Usage.ToolExecutions != 1 {
				t.Errorf("resume receipt=%+v state=%s usage=%+v model=%d tool=%d", receipt, tr.State, tr.Usage, f.model.Calls(), f.runs.Load())
			}
			root := session.rt.opts.fileRoot
			if root == nil {
				t.Fatal("default factory root was not retained")
			}
			if _, err := root.Stat("."); err != nil {
				t.Fatal("runtime closed borrowed root:", err)
			}
			if err := session.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
				t.Errorf("backend did not close transferred root: %v", err)
			}
			if !reflect.DeepEqual(outsideBefore, fileBoundaryTree(t, outside)) {
				t.Error("snapshot or resume changed stale path bytes, modes or entries")
			}
		})
	}
}

func TestFileBoundaryFactoryAttachmentsUseDefaultRoot(t *testing.T) {
	for _, phase := range []string{"Create", "Open"} {
		t.Run(phase, func(t *testing.T) {
			model := &modalModel{FakeModel: testkit.NewFake(testkit.Step{Text: "observed"})}
			fixture, session, sid := attachmentSession(t, model)
			text, _, err := fixture.SaveAttachment(t.Context(), sid, attachmentFixtureRequest{IdempotencyKey: "text", MimeType: "text/plain", Content: []byte("original factory attachment")})
			if err != nil {
				t.Fatal(err)
			}
			image, _, err := fixture.SaveAttachment(t.Context(), sid, attachmentFixtureRequest{IdempotencyKey: "image", MimeType: "image/png", Content: pngBytes})
			if err != nil {
				t.Fatal(err)
			}
			if phase == "Open" {
				if err := session.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				opts := fixture.opts
				opts.SessionID = sid
				session, err = OpenAgentSession(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = session.Close(context.Background()) })
			}
			outside := t.TempDir()
			if _, err := storage.PrepareSessionDir(outside, sid); err != nil {
				t.Fatal(err)
			}
			foreign := *fixture
			foreign.opts.StateRoot = outside
			for _, key := range []string{"text", "image"} {
				rec, _, err := foreign.SaveAttachment(t.Context(), sid, attachmentFixtureRequest{IdempotencyKey: key, MimeType: "text/plain", Content: []byte("foreign self-consistent attachment")})
				if err != nil || key == "text" && rec.ArtifactID != text.ArtifactID || key == "image" && rec.ArtifactID != image.ArtifactID {
					t.Fatal("foreign fixture:", err)
				}
			}
			before := fileBoundaryTree(t, outside)
			if err := session.rt.do(t.Context(), func(rt *runtime) error {
				rt.opts.StateRoot = outside
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			view := session.rt.manager.View()
			_, err = session.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: attachmentInput("image", image.ArtifactID)})
			factoryRequireCode(t, err, product.CodeUnsupportedCapability)
			if session.rt.manager.View().LastSeq != view.LastSeq || model.Calls() != 0 {
				t.Fatal("attachment rejection committed or invoked model")
			}
			receipt, err := session.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: attachmentInput("text", text.ArtifactID)})
			if err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return session.rt.manager.View().Traces[receipt.TraceID].Settled })
			model.mu.Lock()
			var original, stale bool
			for _, input := range model.inputs {
				for _, msg := range input {
					for _, block := range msg.ContentBlocks {
						if block.UserInputText != nil {
							original = original || strings.Contains(block.UserInputText.Text, "original factory attachment")
							stale = stale || strings.Contains(block.UserInputText.Text, "foreign self-consistent attachment")
						}
					}
				}
			}
			model.mu.Unlock()
			if !original || stale || model.Calls() != 1 || len(session.rt.manager.View().Calls) != 0 {
				t.Errorf("expansion original=%v foreign=%v model=%d", original, stale, model.Calls())
			}
			if _, err := session.rt.opts.fileRoot.Stat("."); err != nil {
				t.Fatal("attachment reader closed borrowed root:", err)
			}
			if !reflect.DeepEqual(before, fileBoundaryTree(t, outside)) {
				t.Error("attachment reader changed external bytes, modes or entries")
			}
		})
	}
}

func TestFileBoundaryFactorySingleRootsAndInspectionOwnership(t *testing.T) {
	for _, phase := range []string{"Create", "Open", "ReadOnlyOpen"} {
		t.Run(phase, func(t *testing.T) {
			opts := factoryDiskOptions(t)
			if phase != "Create" {
				factorySeed(t, opts)
				opts.ReadOnly = phase == "ReadOnlyOpen"
			}
			var children []*os.Root
			var retained *storage.ResourceRoots
			var inspection *os.File
			boundCalls := 0
			openChild := func(parent *os.Root, name string) (*os.Root, error) {
				root, err := parent.OpenRoot(name)
				if err == nil {
					children = append(children, root)
				}
				return root, err
			}
			openBound := func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
				boundCalls++
				retained, inspection = roots, file
				if len(children) != 2 || roots.Namespace != children[0] || roots.Resource != children[1] {
					t.Fatal("factory reopened its namespace or resource root")
				}
				if phase == "Create" {
					if file != nil || opt.OpenExisting {
						t.Fatal("Create inspected an existing journal")
					}
				} else {
					if file == nil || !opt.OpenExisting || opt.ReadOnly != opts.ReadOnly {
						t.Fatal("Open lost its retained inspection or mode")
					}
					if _, err := file.Seek(7, io.SeekStart); err != nil {
						t.Fatal(err)
					}
					got, err := readHeaderFromFile(roots.Resource, file, id, opt.MaxLine, product.CodeInvalidArgument)
					if err != nil || !reflect.DeepEqual(got, header) {
						t.Fatalf("same-file header=%+v err=%v", got, err)
					}
					binding, err := decodeBinding(header.Workspace)
					if err != nil || validateCodePrefix(file, id, opt.MaxLine, binding) != nil {
						t.Fatal("same-file complete prefix validation failed")
					}
					if pos, err := file.Seek(0, io.SeekCurrent); err != nil || pos != 7 {
						t.Fatalf("borrowed header/prefix changed caller offset: %d %v", pos, err)
					}
				}
				return jsonl.OpenBound(id, roots, file, header, opt)
			}
			var session *AgentSession
			var err error
			if phase == "Create" {
				session, err = createAgentSessionWithOpen(t.Context(), opts, openChild, openBound)
			} else {
				session, err = openAgentSessionWithOpen(t.Context(), opts, openChild, openBound)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close(context.Background()) })
			if boundCalls != 1 || session.rt.opts.fileRoot != retained.Resource {
				t.Fatal("factory did not transfer the single retained resource root")
			}
			if inspection != nil {
				factoryAssertFileClosed(t, inspection)
			}
			if _, err := session.Snapshot(t.Context()); err != nil {
				t.Fatal(err)
			}
			if opts.Model.(*testkit.FakeModel).Calls() != 0 || len(session.rt.manager.View().Calls) != 0 {
				t.Fatal("factory inspection executed work")
			}
			if err := session.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, root := range children {
				if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
					t.Errorf("Close did not release owned root: %v", err)
				}
			}
		})
	}
}

func TestFileBoundaryFactoryPreCompatibilityHasZeroBackendEffects(t *testing.T) {
	opts := factoryDiskOptions(t)
	factorySeed(t, opts)
	factoryAppendRecord(t, opts, "workflow_node", json.RawMessage(`{"id":"exited"}`))
	dir := filepath.Dir(journalPath(opts.StateRoot, opts.SessionID))
	if err := os.Remove(filepath.Join(dir, "writer.lock")); err != nil {
		t.Fatal(err)
	}
	if goruntime.GOOS != "windows" {
		for path, mode := range map[string]os.FileMode{filepath.Dir(dir): 0750, dir: 0750, filepath.Join(dir, "journal.jsonl"): 0640} {
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := fileBoundaryTree(t, opts.StateRoot)
	var children []*os.Root
	boundCalls := 0
	session, err := openAgentSessionWithOpen(t.Context(), opts, func(parent *os.Root, name string) (*os.Root, error) {
		root, err := parent.OpenRoot(name)
		if err == nil {
			children = append(children, root)
		}
		return root, err
	}, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
		boundCalls++
		return jsonl.OpenBound(id, roots, file, header, opt)
	})
	if session != nil {
		_ = session.Close(t.Context())
		t.Fatal("exited required fact started runtime")
	}
	factoryRequireCode(t, err, product.CodeIncompatibleVersion)
	if boundCalls != 0 || len(children) != 2 || opts.Model.(*testkit.FakeModel).Calls() != 0 || !reflect.DeepEqual(before, fileBoundaryTree(t, opts.StateRoot)) {
		t.Error("pre-compatibility opened backend, invoked model, or changed bytes/modes/entries")
	}
	for _, root := range children {
		if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
			t.Error("compatibility failure leaked factory root:", err)
		}
	}
}

func TestFileBoundaryFactoryClosedInspectionAndRootReleaseAllOwners(t *testing.T) {
	for _, failure := range []string{"closed-inspection", "closed-root", "inspection-close-after-success", "backend-error-with-close-error"} {
		t.Run(failure, func(t *testing.T) {
			opts := factoryDiskOptions(t)
			factorySeed(t, opts)
			var retained *storage.ResourceRoots
			var inspected *os.File
			var opened *jsonl.Store
			calls := 0
			session, err := openAgentSessionWithOpen(t.Context(), opts, nil, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
				calls++
				retained, inspected = roots, file
				switch failure {
				case "closed-root":
					if err := roots.Resource.Close(); err != nil {
						t.Fatal(err)
					}
				case "closed-inspection", "backend-error-with-close-error":
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
					if failure == "backend-error-with-close-error" {
						return nil, errors.Join(product.NewError(product.CodeIncompatibleVersion, "principal backend error"), &os.PathError{Op: "open", Path: opts.StateRoot, Err: os.ErrClosed})
					}
				}
				backend, err := jsonl.OpenBound(id, roots, file, header, opt)
				opened = backend
				if failure == "inspection-close-after-success" && err == nil {
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
				}
				return backend, err
			})
			if session != nil {
				_ = session.Close(t.Context())
				t.Fatal("closed owner started runtime")
			}
			code := product.CodeStorageUnavailable
			if failure == "backend-error-with-close-error" {
				code = product.CodeIncompatibleVersion
			}
			factoryRequireCode(t, err, code)
			if calls != 1 || inspected == nil || retained == nil || opts.Model.(*testkit.FakeModel).Calls() != 0 {
				t.Fatal("fault boundary was not actually invoked once")
			}
			if failure == "inspection-close-after-success" && (opened == nil || opened.ResourceRoot() != nil) {
				t.Error("inspection Close failure did not close successfully opened backend")
			}
			factoryAssertFileClosed(t, inspected)
			for _, root := range []*os.Root{retained.Resource, retained.Namespace} {
				if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
					t.Error("failure leaked retained root:", err)
				}
			}
			factoryAssertLegacyWriterReleased(t, opts)
		})
	}
}

func TestFileBoundaryFactoryCleanupPreservesCompatibilityAndContext(t *testing.T) {
	opts := factoryDiskOptions(t)
	factorySeed(t, opts)
	factoryAppendRecord(t, opts, "workflow_node", json.RawMessage(`{"id":"exited"}`))
	roots, err := storage.OpenResourceRoots(opts.StateRoot, storage.ResourceCode, opts.SessionID, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	file, err := jsonl.OpenJournalReader(roots.Resource)
	if err != nil {
		_ = roots.Close()
		t.Fatal(err)
	}
	header, err := readHeaderFromFile(roots.Resource, file, opts.SessionID, 0, product.CodeInvalidArgument)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := decodeBinding(header.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	principal := validateCodePrefix(file, opts.SessionID, opts.Limits.WithDefaults().MaxCommitLineBytes, binding)
	factoryRequireCode(t, principal, product.CodeIncompatibleVersion)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := os.Open(journalPath(opts.StateRoot, opts.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	got := closeFactoryOwners(principal, file, roots, other)
	factoryRequireCode(t, got, product.CodeIncompatibleVersion)
	if got != principal {
		t.Error("cleanup replaced the original compatibility error")
	}
	factoryAssertFileClosed(t, other)
	if _, err := roots.Namespace.Stat("."); !errors.Is(err, os.ErrClosed) {
		t.Error("cleanup skipped namespace root:", err)
	}
	for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
		got := closeFactoryOwners(errors.Join(ctxErr, &os.PathError{Op: "read", Path: opts.StateRoot, Err: os.ErrClosed}), file)
		if !errors.Is(got, ctxErr) || strings.Contains(got.Error(), opts.StateRoot) {
			t.Errorf("cleanup lost safe recognizable context error: %v", got)
		}
	}
}

func TestFileBoundaryFactoryLaterFailureReleasesWriter(t *testing.T) {
	for _, failure := range []string{"manager", "profile", "registry", "manifest", "model"} {
		t.Run(failure, func(t *testing.T) {
			opts := factoryDiskOptions(t)
			factorySeed(t, opts)
			code := product.CodeInvalidArgument
			switch failure {
			case "manager":
				factoryAppendRecord(t, opts, "unknown_required", json.RawMessage(`{}`))
				code = product.CodeIncompatibleVersion
			case "profile":
				opts.Profile = ProfileDefault
				code = product.CodeResourceUnavailable
			case "registry":
				opts.Agents = []agent.AgentDefinition{{Name: agent.MainAgentName, Version: "duplicate"}}
			case "manifest":
				opts.GenerationFingerprint = "different-fingerprint"
				code = product.CodeIncompatibleVersion
			case "model":
				opts.Model = nil
			}
			var retained *storage.ResourceRoots
			var backend *jsonl.Store
			calls := 0
			session, err := openAgentSessionWithOpen(t.Context(), opts, nil, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
				calls++
				retained = roots
				opened, err := jsonl.OpenBound(id, roots, file, header, opt)
				backend = opened
				return opened, err
			})
			if session != nil {
				_ = session.Close(t.Context())
				t.Fatal("invalid runtime started")
			}
			factoryRequireCode(t, err, code)
			if calls != 1 || backend == nil || backend.ResourceRoot() != nil {
				t.Fatal("later failure did not close its own actual backend")
			}
			for _, root := range []*os.Root{retained.Namespace, retained.Resource} {
				if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
					t.Error("later failure leaked root:", err)
				}
			}
			factoryAssertLegacyWriterReleased(t, opts)
		})
	}
	t.Run("CreateStart", func(t *testing.T) {
		opts := factoryDiskOptions(t)
		opts.Profile = ProfileDefault
		var backend *jsonl.Store
		session, err := createAgentSessionWithOpen(t.Context(), opts, nil, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
			opened, err := jsonl.OpenBound(id, roots, file, header, opt)
			backend = opened
			return opened, err
		})
		if session != nil {
			_ = session.Close(t.Context())
			t.Fatal("unsupported profile started")
		}
		factoryRequireCode(t, err, product.CodeResourceUnavailable)
		if backend == nil || backend.ResourceRoot() != nil {
			t.Fatal("Create Start failure leaked owned backend")
		}
		factoryAssertLegacyWriterReleased(t, opts)
	})
}

func TestFileBoundaryFactoryCanceledBeforeFinalBackend(t *testing.T) {
	for _, phase := range []string{"Create", "Open"} {
		for _, moment := range []string{"before-call", "after-roots"} {
			t.Run(phase+"/"+moment, func(t *testing.T) {
				opts := factoryDiskOptions(t)
				if phase == "Open" {
					factorySeed(t, opts)
				}
				before := fileBoundaryTree(t, opts.StateRoot)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if moment == "before-call" {
					cancel()
				}
				childCalls, boundCalls := 0, 0
				var children []*os.Root
				openChild := func(parent *os.Root, name string) (*os.Root, error) {
					childCalls++
					root, err := parent.OpenRoot(name)
					if err == nil {
						children = append(children, root)
					}
					if name == opts.SessionID {
						cancel()
					}
					return root, err
				}
				openBound := func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
					boundCalls++
					return jsonl.OpenBound(id, roots, file, header, opt)
				}
				var session *AgentSession
				var err error
				if phase == "Create" {
					session, err = createAgentSessionWithOpen(ctx, opts, openChild, openBound)
				} else {
					session, err = openAgentSessionWithOpen(ctx, opts, openChild, openBound)
				}
				if session != nil {
					_ = session.Close(t.Context())
					t.Fatal("canceled factory started runtime")
				}
				if !errors.Is(err, context.Canceled) || boundCalls != 0 || moment == "before-call" && childCalls != 0 || moment == "after-roots" && childCalls != 2 {
					t.Errorf("cancel err=%v children=%d backend=%d", err, childCalls, boundCalls)
				}
				if moment == "before-call" || phase == "Open" {
					if !reflect.DeepEqual(before, fileBoundaryTree(t, opts.StateRoot)) {
						t.Error("pre-backend cancellation changed bytes/modes/entries")
					}
				} else {
					for _, name := range []string{"journal.jsonl", "writer.lock"} {
						if _, err := os.Lstat(filepath.Join(opts.StateRoot, "sessions", opts.SessionID, name)); !os.IsNotExist(err) {
							t.Error("Create cancellation created journal or lock:", err)
						}
					}
				}
				for _, root := range children {
					if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
						t.Error("cancellation leaked root:", err)
					}
				}
			})
		}
	}
}

func TestFileBoundaryFactoryMemoryAndInjectedKeepOriginalContract(t *testing.T) {
	for _, injected := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "injected"}[injected], func(t *testing.T) {
			opts := factoryDiskOptions(t)
			opts.StateRoot = "memory"
			if injected {
				backend, err := memory.Open(opts.SessionID, storage.Header{ResourceType: storage.ResourceCode, Workspace: workspaceJSON(opts.Workspace, "")})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = backend.Close() })
				opts.Store = backend
			}
			calls := 0
			session, err := createAgentSessionWithOpen(t.Context(), opts, nil, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
				calls++
				return jsonl.OpenBound(id, roots, file, header, opt)
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close(context.Background()) })
			if _, err := session.Snapshot(t.Context()); err != nil {
				t.Fatal(err)
			}
			if calls != 0 || session.rt.opts.fileRoot != nil || opts.Model.(*testkit.FakeModel).Calls() != 0 || injected && session.rt.opts.Store != opts.Store {
				t.Error("memory/injected path unexpectedly opened or borrowed a file backend")
			}
		})
	}
}

func TestFileBoundaryFactoryFinalOpenUsesInspectedIdentity(t *testing.T) {
	for _, component := range []string{"journal", "session", "namespace"} {
		t.Run(component, func(t *testing.T) {
			opts := factoryDiskOptions(t)
			factorySeed(t, opts)
			foreignOpts := factoryDiskOptions(t)
			foreignOpts.SessionID = opts.SessionID
			foreignOpts.GenerationFingerprint = "foreign-bundle"
			factorySeed(t, foreignOpts)
			original := journalPath(opts.StateRoot, opts.SessionID)
			replacement := journalPath(foreignOpts.StateRoot, foreignOpts.SessionID)
			if component != "journal" {
				original, replacement = filepath.Dir(original), filepath.Dir(replacement)
				if component == "namespace" {
					original, replacement = filepath.Dir(original), filepath.Dir(replacement)
				}
			}
			before := fileBoundaryTree(t, replacement)
			foreignNow := replacement
			moved := false
			calls := 0
			var retained *storage.ResourceRoots
			session, err := openAgentSessionWithOpen(t.Context(), opts, nil, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
				calls++
				retained = roots
				renameErr := os.Rename(original, original+".held")
				if renameErr != nil {
					var errno syscall.Errno
					if goruntime.GOOS != "windows" || !errors.As(renameErr, &errno) || errno != 5 && errno != 32 {
						t.Fatal("actual retained-binding rename fixture:", renameErr)
					}
					t.Logf("Windows actual %s rename refused errno=%d; original binding retained", component, errno)
				} else {
					if err := os.Rename(replacement, original); err != nil {
						t.Fatal("ordinary replacement move:", err)
					}
					moved, foreignNow = true, original
					t.Logf("%s actual %s move succeeded; replacement is ordinary, retained roots/file must not follow it", goruntime.GOOS, component)
				}
				return jsonl.OpenBound(id, roots, file, header, opt)
			})
			if moved && component == "journal" {
				if session != nil {
					_ = session.Close(t.Context())
					t.Fatal("replacement journal became the inspected writer")
				}
				factoryRequireCode(t, err, product.CodeInvalidArgument)
			} else {
				if err != nil {
					t.Fatal("retained directory/file binding:", err)
				}
				t.Cleanup(func() { _ = session.Close(context.Background()) })
				if _, err := session.Snapshot(t.Context()); err != nil || session.rt.opts.Workspace != opts.Workspace {
					t.Fatal("final store/header changed original binding:", err)
				}
				if err := session.rt.do(t.Context(), func(rt *runtime) error { return matchGeneration(&rt.opts, rt.generation) }); err != nil {
					t.Fatal("final runtime followed foreign manifest:", err)
				}
				if err := session.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 1 || opts.Model.(*testkit.FakeModel).Calls() != 0 || foreignOpts.Model.(*testkit.FakeModel).Calls() != 0 {
				t.Error("identity observation retried backend or executed model")
			}
			if !reflect.DeepEqual(before, fileBoundaryTree(t, foreignNow)) {
				t.Error("factory changed replacement bytes, modes or entries")
			}
			for _, root := range []*os.Root{retained.Namespace, retained.Resource} {
				if _, err := root.Stat("."); !errors.Is(err, os.ErrClosed) {
					t.Error("final binding owner leaked root:", err)
				}
			}
		})
	}
}

func TestFileBoundaryFactoryFinalWorkspaceBindingChangeFailsClosed(t *testing.T) {
	opts := factoryDiskOptions(t)
	factorySeed(t, opts)
	journal := journalPath(opts.StateRoot, opts.SessionID)
	raw, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	parts := bytes.SplitN(raw, []byte("\n"), 2)
	var header storage.Header
	if err := json.Unmarshal(parts[0], &header); err != nil {
		t.Fatal(err)
	}
	foreignWorkspace := t.TempDir()
	workspaceBefore := fileBoundaryTree(t, foreignWorkspace)
	binding, err := decodeBinding(header.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	header.Workspace = workspaceJSON(foreignWorkspace, binding.Generation)
	replacedHeader, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	changed := append(append(replacedHeader, '\n'), parts[1]...)
	calls := 0
	var backend *jsonl.Store
	session, err := openAgentSessionWithOpen(t.Context(), opts, nil, func(id string, roots *storage.ResourceRoots, file *os.File, inspectedHeader storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
		calls++
		// A real write changes the same inode after Header/prefix validation.
		// OpenBound's identity check alone cannot detect a different binding.
		if err := os.WriteFile(journal, changed, 0600); err != nil {
			t.Fatal(err)
		}
		opened, err := jsonl.OpenBound(id, roots, file, inspectedHeader, opt)
		backend = opened
		return opened, err
	})
	if session != nil {
		_ = session.Close(t.Context())
		t.Fatal("mutated final binding started runtime")
	}
	factoryRequireCode(t, err, product.CodeIncompatibleVersion)
	if calls != 1 || backend == nil || backend.ResourceRoot() != nil || opts.Model.(*testkit.FakeModel).Calls() != 0 {
		t.Error("final binding rejection did not close actual backend or executed model")
	}
	after, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(after, changed) || !reflect.DeepEqual(workspaceBefore, fileBoundaryTree(t, foreignWorkspace)) {
		t.Error("final binding rejection changed bytes or foreign workspace")
	}
	factoryAssertLegacyWriterReleased(t, opts)
}

func TestFileBoundaryFactoryCreateExistingLeafAndSaveFailure(t *testing.T) {
	for _, kind := range []string{"ordinary-journal", "directory-journal", "manifest-save-failure"} {
		t.Run(kind, func(t *testing.T) {
			opts := factoryDiskOptions(t)
			dir, err := storage.PrepareSessionDir(opts.StateRoot, opts.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			leaf := filepath.Join(dir, "journal.jsonl")
			if kind == "manifest-save-failure" {
				leaf = filepath.Join(dir, "resources")
			}
			if kind == "directory-journal" {
				err = os.Mkdir(leaf, 0700)
			} else {
				err = os.WriteFile(leaf, []byte("original fixture leaf"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := fileBoundaryTree(t, opts.StateRoot)
			var backend *jsonl.Store
			calls := 0
			session, err := createAgentSessionWithOpen(t.Context(), opts, nil, func(id string, roots *storage.ResourceRoots, file *os.File, header storage.Header, opt jsonl.Options) (*jsonl.Store, error) {
				calls++
				opened, err := jsonl.OpenBound(id, roots, file, header, opt)
				backend = opened
				return opened, err
			})
			if session != nil {
				_ = session.Close(t.Context())
				t.Fatal("failed Create started runtime")
			}
			if kind == "manifest-save-failure" {
				factoryRequireCode(t, err, product.CodeInvalidArgument)
				if calls != 1 || backend == nil || backend.ResourceRoot() != nil {
					t.Fatal("legacy save failure leaked default bound backend")
				}
				factoryAssertLegacyWriterReleased(t, opts)
			} else {
				factoryRequireCode(t, err, product.CodeStateConflict)
				if calls != 0 || !reflect.DeepEqual(before, fileBoundaryTree(t, opts.StateRoot)) {
					t.Error("existing journal rejection opened backend or changed existing data")
				}
			}
			if opts.Model.(*testkit.FakeModel).Calls() != 0 {
				t.Error("failed Create invoked model")
			}
		})
	}
}

func TestFileBoundaryFactoryOpenStillIgnoresInjectedStore(t *testing.T) {
	opts := factoryDiskOptions(t)
	factorySeed(t, opts)
	injected, err := memory.Open("unrelated", storage.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer injected.Close()
	opts.Store = injected
	session, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal("file Open changed to injected-store semantics:", err)
	}
	defer session.Close(context.Background())
	if snap, err := session.Snapshot(t.Context()); err != nil || snap.SessionID != opts.SessionID || session.rt.opts.Store == injected {
		t.Error("Open did not use the original file journal:", err)
	}
	stored, err := injected.Load(t.Context(), "unrelated")
	if err != nil || len(stored.Commits) != 0 || opts.Model.(*testkit.FakeModel).Calls() != 0 {
		t.Error("file Open touched unrelated injected store or invoked model")
	}
}

func factoryDiskOptions(t *testing.T) Options {
	t.Helper()
	return Options{SessionID: "factory-binding", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: testkit.NewFake(), GenerationFingerprint: "factory-bundle"}
}

func factorySeed(t *testing.T, opts Options) {
	t.Helper()
	session, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal("seed:", err)
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func factoryAppendRecord(t *testing.T, opts Options, kind string, payload json.RawMessage) {
	t.Helper()
	backend, err := jsonl.Open(opts.SessionID, opts.StateRoot, storage.Header{}, jsonl.Options{OpenExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	stored, err := backend.Load(t.Context(), opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.Append(t.Context(), opts.SessionID, storage.ExpectedCommit{ExpectedPreviousSeq: stored.LastSeq}, storage.Commit{CommitID: agent.MustID(), ExpectedPreviousSeq: stored.LastSeq,
		ControlRecords: []storage.Record{{Type: kind, Version: 1, ID: agent.MustID(), Payload: payload}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func factoryAssertLegacyWriterReleased(t *testing.T, opts Options) {
	t.Helper()
	backend, err := jsonl.Open(opts.SessionID, opts.StateRoot, storage.Header{}, jsonl.Options{OpenExisting: true})
	if err != nil {
		t.Fatal("legacy writer still competes with failed factory:", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
}

func factoryAssertFileClosed(t *testing.T, file *os.File) {
	t.Helper()
	// File.Stat on Windows reports ERROR_INVALID_HANDLE after Close rather
	// than os.ErrClosed. Read checks the descriptor's closed state on both OSes.
	var one [1]byte
	if _, err := file.Read(one[:]); !errors.Is(err, os.ErrClosed) {
		t.Error("factory leaked owned inspection/file:", err)
	}
}

func factoryRequireCode(t *testing.T, err error, code string) {
	t.Helper()
	if pe, ok := product.AsError(err); !ok || pe.Code != code || pe.Details != nil || len(pe.Refs) != 0 {
		t.Fatalf("error=%v, want direct %s without details/refs", err, code)
	}
}
