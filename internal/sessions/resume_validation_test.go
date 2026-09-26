package sessions

import (
	"context"
	"reflect"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

type unreadableResumeBlobs struct {
	store.Store
	store.CheckpointBlobs
}

func (unreadableResumeBlobs) Get(context.Context, string, store.BlobRef) ([]byte, error) {
	return nil, product.NewError(product.CodeStorageUnavailable, "checkpoint unavailable")
}

func TestResumeRejectsConfigurationChangedBetweenAttemptAndPause(t *testing.T) {
	f := pausedResumeFixture(t, false, resumeFixtureOptions{ChangingVersion: true})
	before := f.manager.View()
	_, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleResume || !reflect.DeepEqual(before, f.manager.View()) {
		t.Fatalf("model configuration changed within the paused attempt: resume=%v", err)
	}
	if f.model.Calls() != 1 || f.runs.Load() != 0 || len(f.manager.View().Operations) != len(before.Operations) {
		t.Fatal("model version rejection executed or accepted work")
	}
	snap, err := f.s.Snapshot(t.Context())
	if err != nil || snap.Resume[f.input.TraceID].CanResume || snap.Resume[f.input.TraceID].Code != product.CodeIncompatibleResume {
		t.Fatalf("changed model eligibility=%+v err=%v", snap.Resume, err)
	}
}

func TestResumeRejectsUnavailableImplementationWithoutStartingWork(t *testing.T) {
	f := pausedResumeFixture(t, false)
	if err := f.s.rt.do(t.Context(), func(rt *runtime) error { rt.opts.Tools[0].Run = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	before := f.manager.View()
	_, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleResume {
		t.Fatalf("missing implementation resume=%v", err)
	}
	if !reflect.DeepEqual(before, f.manager.View()) || f.model.Calls() != 1 || f.runs.Load() != 0 {
		t.Fatal("missing implementation started work or committed acceptance")
	}
}

func TestResumeRejectsIncompatibleBindingsAndMissingBlob(t *testing.T) {
	cases := []struct {
		name   string
		change func(*runtime)
	}{
		{"workspace", func(rt *runtime) { rt.opts.Workspace += "-changed" }},
		{"environment", func(rt *runtime) { rt.opts.ResourceEnvironment = "other-backend" }},
		{"build-declaration", func(rt *runtime) { rt.opts.GenerationFingerprint += "-new" }},
		{"missing-declaration", func(rt *runtime) { rt.opts.GenerationFingerprint = "" }},
		{"generation", func(rt *runtime) { rt.generation = "other-generation" }},
		{"instruction", func(rt *runtime) { rt.opts.Instruction = "changed" }},
		{"tool-version", func(rt *runtime) { rt.opts.Tools[0].Version = "new" }},
		{"tool-interface", func(rt *runtime) { rt.opts.Tools[0].ToolInterface = "enhanced-invokable" }},
		{"missing-blob", func(rt *runtime) {
			rt.opts.Store = unreadableResumeBlobs{rt.opts.Store, rt.opts.Store.(store.CheckpointBlobs)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := pausedResumeFixture(t, false)
			if err := f.s.rt.do(t.Context(), func(rt *runtime) error { tc.change(rt); return nil }); err != nil {
				t.Fatal(err)
			}
			before := f.manager.View()
			_, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleResume {
				t.Fatalf("resume=%v", err)
			}
			if !reflect.DeepEqual(before, f.manager.View()) || f.model.Calls() != 1 || f.runs.Load() != 0 {
				t.Fatal("incompatible resume started work or changed state")
			}
			snap, err := f.s.Snapshot(t.Context())
			if err != nil || snap.Resume[f.input.TraceID].CanResume || snap.Resume[f.input.TraceID].Code != product.CodeIncompatibleResume {
				t.Fatalf("snapshot eligibility=%+v err=%v", snap.Resume, err)
			}
		})
	}
}

func TestResumeRechecksRevokedPolicyAndReadOnlyAccess(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{true: "read-only-session", false: "revoked-policy"}[readOnly], func(t *testing.T) {
			f := pausedResumeFixture(t, false, resumeFixtureOptions{Effect: "write"})
			if err := f.s.rt.do(t.Context(), func(rt *runtime) error {
				if readOnly {
					rt.opts.ReadOnly = true
					return nil
				}
				policy := rt.manager.View().ExecutionPolicy
				policy.SandboxMode = "read-only"
				return rt.manager.SetExecutionPolicy(t.Context(), policy.Revision, policy)
			}); err != nil {
				t.Fatal(err)
			}
			before := f.manager.View()
			_, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodePermissionDenied || !reflect.DeepEqual(before, f.manager.View()) {
				t.Fatalf("permission revoked resume=%v", err)
			}
			if f.model.Calls() != 1 || f.runs.Load() != 0 {
				t.Fatal("revoked resume started work")
			}
		})
	}
}
