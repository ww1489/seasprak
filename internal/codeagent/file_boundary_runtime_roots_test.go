package codeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

// These tests supply the checked borrowed root through the existing internal
// mailbox. They cover runtime selection, not the still-pending factory transfer
// of its own default backend root into Options.
func TestFileBoundaryRuntimeManifestUsesBorrowedRoot(t *testing.T) {
	f := pausedResumeFixture(t, false, resumeFixtureOptions{Disk: true})
	roots, err := storage.OpenResourceRoots(f.opts.StateRoot, storage.ResourceCode, f.opts.SessionID, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.s.Close(context.Background()); _ = roots.Close() })
	outside := t.TempDir()
	outsideBefore := fileBoundaryTree(t, outside)
	if err := f.s.rt.do(t.Context(), func(rt *runtime) error {
		rt.opts.fileRoot, rt.opts.StateRoot = roots.Resource, outside
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := f.manager.View()
	cp := before.Checkpoints[before.Traces[f.input.TraceID].CheckpointID]
	if err := f.s.rt.do(t.Context(), func(rt *runtime) error {
		frame := &execution{scope: cp.Scope, turnID: cp.Scope.TurnID, turnSelectionRevision: cp.SelectionRevision}
		got, err := rt.pauseReference(frame, cp.Input, agent.CheckpointBlobRef{Hash: cp.BlobHash, Size: cp.BlobSize}, rt.manager.View())
		if err != nil {
			return err
		}
		if got.ManifestHash != cp.ManifestHash || got.Scope != cp.Scope {
			t.Error("pause reference did not retain the original manifest and execution binding")
		}
		return nil
	}); err != nil {
		t.Error("pause reference reopened the stale diagnostic path:", err)
	}
	snap, err := f.s.Snapshot(t.Context())
	if err != nil || !snap.Resume[f.input.TraceID].CanResume {
		t.Errorf("snapshot did not use the borrowed manifest: eligibility=%+v err=%v", snap.Resume[f.input.TraceID], err)
	}
	if f.manager.View().LastSeq != before.LastSeq || f.model.Calls() != 1 || f.runs.Load() != 0 {
		t.Fatal("manifest observation committed or executed work")
	}
	if _, err := roots.Resource.Stat("."); err != nil {
		t.Fatal("runtime manifest read closed the borrowed root:", err)
	}
	receipt, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
	if err != nil {
		t.Error("resume did not use the borrowed manifest:", err)
	} else {
		waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
		after := f.manager.View()
		trace := after.Traces[f.input.TraceID]
		if receipt.State != "accepted" || trace.State != "completed" || !trace.Settled || f.model.Calls() != 2 || f.runs.Load() != 1 {
			t.Errorf("resume state=%s settled=%v receipt=%s model=%d tool=%d", trace.State, trace.Settled, receipt.State, f.model.Calls(), f.runs.Load())
		}
		if trace.Usage.LogicalModelCalls != 2 || trace.Usage.TransportRequests != 2 || trace.Usage.ToolExecutions != 1 {
			t.Errorf("resume changed the saved budget: %+v", trace.Usage)
		}
	}
	if !reflect.DeepEqual(outsideBefore, fileBoundaryTree(t, outside)) {
		t.Error("runtime manifest lookup changed the stale path tree")
	}
}

func TestFileBoundaryRuntimeClosedManifestRootFailsWithoutPathFallback(t *testing.T) {
	f := pausedResumeFixture(t, false, resumeFixtureOptions{Disk: true})
	roots, err := storage.OpenResourceRoots(f.opts.StateRoot, storage.ResourceCode, f.opts.SessionID, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.s.Close(context.Background()); _ = roots.Close() })
	if err := f.s.rt.do(t.Context(), func(rt *runtime) error {
		rt.opts.fileRoot = roots.Resource
		return roots.Resource.Close()
	}); err != nil {
		t.Fatal("closed-binding fixture:", err)
	}
	before := f.manager.View()
	journalPath := filepath.Join(f.opts.StateRoot, "sessions", f.opts.SessionID, "journal.jsonl")
	journalBefore, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := f.s.Snapshot(t.Context())
	eligibility := snap.Resume[f.input.TraceID]
	if err != nil || eligibility.CanResume || eligibility.Code != product.CodeIncompatibleResume {
		t.Errorf("closed root fell back to a valid absolute path: eligibility=%+v err=%v", eligibility, err)
	}
	receipt, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleResume || receipt.OperationID != "" {
		t.Errorf("closed root resume=%+v err=%v, want zero acceptance/incompatible_resume", receipt, err)
	}
	if err == nil {
		waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	}
	journalAfter, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if f.manager.View().LastSeq != before.LastSeq || f.model.Calls() != 1 || f.runs.Load() != 0 || string(journalBefore) != string(journalAfter) {
		t.Error("closed-root rejection wrote or executed work")
	}
}

func TestFileBoundaryRuntimeAttachmentsUseBorrowedRoot(t *testing.T) {
	model := &modalModel{FakeModel: testkit.NewFake(testkit.Step{Text: "observed attachment"})}
	fixture, session, sid := attachmentSession(t, model)
	text, _, err := fixture.SaveAttachment(t.Context(), sid, attachmentFixtureRequest{IdempotencyKey: "bound-text", MimeType: "text/plain", Content: []byte("original borrowed attachment")})
	if err != nil {
		t.Fatal(err)
	}
	image, _, err := fixture.SaveAttachment(t.Context(), sid, attachmentFixtureRequest{IdempotencyKey: "bound-image", MimeType: "image/png", Content: pngBytes})
	if err != nil {
		t.Fatal(err)
	}
	roots, err := storage.OpenResourceRoots(fixture.opts.StateRoot, storage.ResourceCode, sid, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()); _ = roots.Close() })
	outside := t.TempDir()
	if _, err := storage.PrepareSessionDir(outside, sid); err != nil {
		t.Fatal(err)
	}
	attachments, _, err := storage.SessionSubdir(outside, sid, "attachments", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []storage.Attachment{text, image} {
		data := []byte("foreign self-consistent attachment")
		sum := sha256.Sum256(data)
		rec.MimeType, rec.Size, rec.SHA256 = "text/plain", int64(len(data)), hex.EncodeToString(sum[:])
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := attachments.WriteFile(rec.ArtifactID+".bin", data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := attachments.WriteFile(rec.ArtifactID+".json", raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := attachments.Close(); err != nil {
		t.Fatal(err)
	}
	outsideBefore := fileBoundaryTree(t, outside)
	if err := session.rt.do(t.Context(), func(rt *runtime) error {
		rt.opts.fileRoot, rt.opts.StateRoot = roots.Resource, outside
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	receipt, err := session.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: attachmentInput("read", text.ArtifactID)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return session.rt.manager.View().Traces[receipt.TraceID].Settled })
	model.mu.Lock()
	var original, foreign bool
	for _, input := range model.inputs {
		for _, message := range input {
			for _, block := range message.ContentBlocks {
				if block.UserInputText != nil {
					original = original || strings.Contains(block.UserInputText.Text, "original borrowed attachment")
					foreign = foreign || strings.Contains(block.UserInputText.Text, "foreign self-consistent attachment")
				}
			}
		}
	}
	model.mu.Unlock()
	if !original || foreign || model.Calls() != 1 {
		t.Errorf("model expansion reopened a path: original=%v foreign=%v calls=%d", original, foreign, model.Calls())
	}
	before := session.rt.manager.View()
	_, err = session.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: attachmentInput("image", image.ArtifactID)})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeUnsupportedCapability {
		t.Error("admission did not validate the original image against model capabilities:", err)
	}
	if err == nil {
		waitFor(t, func() bool {
			for _, tr := range session.rt.manager.View().Traces {
				if !tr.Settled {
					return false
				}
			}
			return true
		})
	}
	if session.rt.manager.View().LastSeq != before.LastSeq || model.Calls() != 1 {
		t.Error("borrowed attachment rejection committed or invoked the model")
	}
	if _, err := roots.Resource.Stat("."); err != nil {
		t.Fatal("attachment resolution closed the borrowed root:", err)
	}
	if !reflect.DeepEqual(outsideBefore, fileBoundaryTree(t, outside)) {
		t.Error("runtime attachment lookup changed the outside fixture")
	}
}
