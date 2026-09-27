package operations_test

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

// This sink exercises real Executor issuance and binding only. It has no live
// Session lifecycle validator; lifecycle invalidation is covered in sessions.
type sink struct{}

func (sink) CommitFact(context.Context, agent.ExecutionScope, agent.Fact) error { return nil }

type allow struct{}

func (allow) Authorize(context.Context, agent.FrozenCall) (agent.Decision, error) {
	return agent.DecisionAllow, nil
}

func run(t *testing.T, m *fixture.Memory, name string, args any, files agent.FileOperations, artifacts agent.ArtifactStore) tools.Outcome {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var def tools.Definition
	for _, d := range tools.NewBuiltinDefinitions(tools.BuiltinOptions{}) {
		if d.Name == name {
			def = d
		}
	}
	if name == "save" {
		def = tools.Definition{Name: name, Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "artifact-store", Effect: "write"}}
	}
	e, err := tools.NewExecutor("test", []tools.Definition{def}, sink{}, allow{}, agent.NewBudget(config.DefaultLimits()), tools.WithOperations(tools.Operations{Files: files, Artifacts: artifacts}), tools.WithResourceScheduler(tools.NewResourceScheduler()), tools.WithResourceDomain("test-memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.RunDirect(t.Context(), agent.ExecutionScope{SessionID: "session"}, agent.MustID(), name, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func code(t *testing.T, err error, want string) {
	t.Helper()
	pe, ok := product.AsError(err)
	if !ok || pe.Code != want {
		t.Fatalf("error=%v want=%s", err, want)
	}
}

type filesProbe struct {
	*fixture.Memory
	write func(context.Context, agent.AuthorizedFileWrite) (agent.FileEffect, error)
	edit  func(context.Context, agent.AuthorizedFileEdit) (agent.FileEffect, error)
}

func (p *filesProbe) Write(ctx context.Context, r agent.AuthorizedFileWrite) (agent.FileEffect, error) {
	return p.write(ctx, r)
}
func (p *filesProbe) Edit(ctx context.Context, r agent.AuthorizedFileEdit) (agent.FileEffect, error) {
	return p.edit(ctx, r)
}

type artifactProbe struct {
	*fixture.Memory
	request agent.ArtifactInput
	ref     agent.ArtifactRef
	mutate  func(*agent.ArtifactInput)
	check   func(error)
}

func (p *artifactProbe) Save(ctx context.Context, r agent.ArtifactInput) (agent.ArtifactRef, error) {
	p.request = r
	if p.mutate != nil {
		p.mutate(&r)
	}
	ref, err := p.Memory.Save(ctx, r)
	if p.check != nil {
		p.check(err)
	}
	p.ref = ref
	return ref, err
}

func TestMemoryFilesThroughExecutor(t *testing.T) {
	m := fixture.NewMemory()
	bytes := []byte("old old")
	ref := m.RegisterContent(bytes)
	bytes[0] = 'X'
	var saved agent.AuthorizedFileWrite
	p := &filesProbe{Memory: m}
	p.write = func(ctx context.Context, r agent.AuthorizedFileWrite) (agent.FileEffect, error) {
		saved = r
		return m.Write(ctx, r)
	}
	out := run(t, m, "write_file", map[string]any{"path": "file", "contentRef": ref}, p, m)
	if out.Status != "succeeded" || !out.Executed || out.SideEffect != "confirmed" || m.Calls("write") != 1 {
		t.Fatalf("out=%+v writes=%d", out, m.Calls("write"))
	}
	data, version, ok := m.Snapshot("file")
	if !ok || string(data) != "old old" || version == "" {
		t.Fatal("write lost full content or version")
	}
	data[0] = 'Y'
	_, err := m.Write(t.Context(), saved)
	code(t, err, product.CodePermissionDenied)
	replayedData, replayedVersion, exists := m.Snapshot("file")
	if !exists || string(replayedData) != "old old" || replayedVersion != version || m.Calls("write") != 2 {
		t.Fatal("copied write changed content or version")
	}
	patch := m.RegisterPatch(fixture.Patch{Old: "old", New: "new"})
	p.edit = func(ctx context.Context, r agent.AuthorizedFileEdit) (agent.FileEffect, error) {
		effect, err := m.Edit(ctx, r)
		code(t, err, product.CodeStateConflict)
		if effect.Confirmed || effect.SideEffect != "none" {
			t.Fatalf("effect=%+v", effect)
		}
		return effect, err
	}
	out = run(t, m, "edit_file", map[string]any{"path": "file", "patchRef": patch, "expectedVersion": version}, p, m)
	if out.Status != "failed" || out.Executed {
		t.Fatalf("ambiguous edit=%+v", out)
	}
	patch = m.RegisterPatch(fixture.Patch{Old: "old", New: "new", ReplaceAll: true})
	p.edit = func(ctx context.Context, r agent.AuthorizedFileEdit) (agent.FileEffect, error) { return m.Edit(ctx, r) }
	out = run(t, m, "edit_file", map[string]any{"path": "file", "patchRef": patch, "expectedVersion": version}, p, m)
	data, next, _ := m.Snapshot("file")
	if out.Status != "succeeded" || string(data) != "new new" || next == version {
		t.Fatalf("edit=%+v content=%q", out, data)
	}
	out = run(t, m, "read_file", map[string]any{"path": "file", "offset": 4, "limit": 3}, m, m)
	if out.Content != "new" || out.Status != "succeeded" || m.Calls("read") != 1 || m.Calls("open") != 1 {
		t.Fatalf("read=%+v", out)
	}
}

func TestMemoryRejectsTamperingMissingContentAndVersionConflict(t *testing.T) {
	for _, mode := range []string{"path", "content", "version", "frozen", "cross-request", "missing", "stale"} {
		t.Run(mode, func(t *testing.T) {
			m := fixture.NewMemory()
			version := m.SeedFile("file", []byte("before"))
			ref := m.RegisterContent([]byte("after"))
			p := &filesProbe{Memory: m}
			var original agent.AuthorizedFileWrite
			p.write = func(ctx context.Context, r agent.AuthorizedFileWrite) (agent.FileEffect, error) {
				original = r
				switch mode {
				case "path":
					r.Path = "other"
				case "content":
					r.ContentRef = "other"
				case "version":
					r.ExpectedVersion = "other"
				case "frozen":
					r.Authorization.Frozen.CallID = "other"
				case "cross-request":
					r.Authorization.Frozen.FinalArguments = json.RawMessage(`{"path":"other","contentRef":"other"}`)
				}
				effect, err := m.Write(ctx, r)
				want := product.CodePermissionDenied
				if mode == "missing" {
					want = product.CodeNotFound
				}
				if mode == "stale" {
					want = product.CodeStateConflict
				}
				code(t, err, want)
				return effect, err
			}
			if mode == "missing" {
				ref = "missing"
			}
			if mode == "stale" {
				version = "stale"
			}
			out := run(t, m, "write_file", map[string]any{"path": "file", "contentRef": ref, "expectedVersion": version}, p, m)
			data, _, _ := m.Snapshot("file")
			if out.Executed || out.SideEffect != "none" || string(data) != "before" {
				t.Fatalf("out=%+v data=%q", out, data)
			}
			if mode == "path" {
				effect, err := m.Write(t.Context(), original)
				if err != nil || !effect.Confirmed {
					t.Fatalf("binding rejection consumed ticket: %v", err)
				}
			}
		})
	}
}

func TestIndependentArtifactSaveOpenAndIsolation(t *testing.T) {
	m := fixture.NewMemory()
	content := m.RegisterContent([]byte("full output"))
	p := &artifactProbe{Memory: m}
	out := run(t, m, "save", map[string]any{"operation": "save", "contentRef": content, "mediaType": "text/plain", "name": "log"}, nil, p)
	if out.Status != "succeeded" || !p.ref.Available || p.ref.Size != 11 || p.ref.Hash == "" || m.Calls("save") != 1 {
		t.Fatalf("out=%+v ref=%+v", out, p.ref)
	}
	// Open reads this already-saved result using its exact recorded authority;
	// it does not reuse that authority to start another save or process.
	req := agent.ArtifactRead{Authorization: p.request.Authorization, Ref: p.ref}
	reader, err := m.Open(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(data) != "full output" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	_, err = m.Save(t.Context(), p.request)
	code(t, err, product.CodePermissionDenied)
	req.Authorization.Frozen.Scope.SessionID = "other"
	_, err = m.Open(t.Context(), req)
	code(t, err, product.CodePermissionDenied)
	req.Authorization = p.request.Authorization
	req.Ref.Hash = "wrong"
	_, err = m.Open(t.Context(), req)
	code(t, err, product.CodePermissionDenied)
	other := fixture.NewMemory()
	_, err = other.Open(t.Context(), agent.ArtifactRead{Authorization: p.request.Authorization, Ref: p.ref})
	code(t, err, product.CodeNotFound)
}

func TestIndependentArtifactRejectsMissingAndChangedContent(t *testing.T) {
	for _, mode := range []string{"missing", "changed"} {
		t.Run(mode, func(t *testing.T) {
			m := fixture.NewMemory()
			ref := m.RegisterContent([]byte("complete"))
			p := &artifactProbe{Memory: m}
			want := product.CodeNotFound
			if mode == "missing" {
				ref = "missing"
			} else {
				want = product.CodePermissionDenied
				p.mutate = func(r *agent.ArtifactInput) { r.ContentRef = "changed" }
			}
			p.check = func(err error) { code(t, err, want) }
			out := run(t, m, "save", map[string]any{"operation": "save", "contentRef": ref, "name": "log", "mediaType": "text/plain"}, nil, p)
			if out.Status != "failed" || p.ref.Available || p.ref.ID != "" || m.Calls("save") != 1 {
				t.Fatalf("out=%+v artifact=%+v", out, p.ref)
			}
		})
	}
}

func TestMemoryRejectsDifferentInstalledBackend(t *testing.T) {
	m := fixture.NewMemory()
	other := fixture.NewMemory()
	ref := m.RegisterContent([]byte("body"))
	p := &filesProbe{Memory: m, write: func(ctx context.Context, r agent.AuthorizedFileWrite) (agent.FileEffect, error) {
		effect, err := other.Write(ctx, r)
		code(t, err, product.CodePermissionDenied)
		return effect, err
	}}
	out := run(t, m, "write_file", map[string]any{"path": "file", "contentRef": ref}, p, m)
	if out.Executed || other.Calls("write") != 1 {
		t.Fatalf("out=%+v calls=%d", out, other.Calls("write"))
	}
}

func TestMemoryEditRejectsInvalidRequests(t *testing.T) {
	for _, mode := range []string{"path", "patch", "version", "missing", "stale", "no-match", "empty-old", "copy"} {
		t.Run(mode, func(t *testing.T) {
			m := fixture.NewMemory()
			version := m.SeedFile("file", []byte("before"))
			patch := fixture.Patch{Old: "before", New: "after"}
			if mode == "no-match" {
				patch.Old = "absent"
			}
			if mode == "empty-old" {
				patch.Old = ""
			}
			ref := m.RegisterPatch(patch)
			if mode == "missing" {
				ref = "missing"
			}
			if mode == "stale" {
				version = "stale"
			}
			var saved agent.AuthorizedFileEdit
			p := &filesProbe{Memory: m, edit: func(ctx context.Context, r agent.AuthorizedFileEdit) (agent.FileEffect, error) {
				saved = r
				switch mode {
				case "path":
					r.Path = "other"
				case "patch":
					r.PatchRef = "other"
				case "version":
					r.ExpectedVersion = "other"
				}
				effect, err := m.Edit(ctx, r)
				if mode == "copy" {
					if err != nil {
						t.Fatal(err)
					}
					return effect, err
				}
				want := product.CodePermissionDenied
				switch mode {
				case "missing":
					want = product.CodeNotFound
				case "stale", "no-match":
					want = product.CodeStateConflict
				case "empty-old":
					want = product.CodeInvalidArgument
				}
				code(t, err, want)
				return effect, err
			}}
			out := run(t, m, "edit_file", map[string]any{"path": "file", "patchRef": ref, "expectedVersion": version}, p, m)
			data, beforeCopyVersion, _ := m.Snapshot("file")
			if mode == "copy" {
				_, err := m.Edit(t.Context(), saved)
				code(t, err, product.CodePermissionDenied)
				afterCopy, afterCopyVersion, exists := m.Snapshot("file")
				if !exists || string(afterCopy) != string(data) || afterCopyVersion != beforeCopyVersion {
					t.Fatal("copied edit changed content or version")
				}
				if string(data) != "after" || !out.Executed || m.Calls("edit") != 2 {
					t.Fatalf("out=%+v data=%q", out, data)
				}
			} else if out.Executed || out.SideEffect != "none" || string(data) != "before" || m.Calls("edit") != 1 {
				t.Fatalf("out=%+v data=%q", out, data)
			}
		})
	}
}

func TestMemoryRejectsTicketFromAnotherRealExecutorRequest(t *testing.T) {
	m := fixture.NewMemory()
	ref := m.RegisterContent([]byte("body"))
	var first agent.AuthorizedFileWrite
	p := &filesProbe{Memory: m, write: func(_ context.Context, r agent.AuthorizedFileWrite) (agent.FileEffect, error) {
		first = r
		return agent.FileEffect{SideEffect: "none"}, product.NewError(product.CodeResourceUnavailable, "test pre-start stop")
	}}
	run(t, m, "write_file", map[string]any{"path": "first", "contentRef": ref}, p, m)
	p.write = func(ctx context.Context, r agent.AuthorizedFileWrite) (agent.FileEffect, error) {
		r.Authorization.Ticket = first.Authorization.Ticket
		effect, err := m.Write(ctx, r)
		code(t, err, product.CodePermissionDenied)
		return effect, err
	}
	out := run(t, m, "write_file", map[string]any{"path": "second", "contentRef": ref}, p, m)
	if out.Executed || out.SideEffect != "none" || m.Calls("write") != 1 {
		t.Fatalf("out=%+v", out)
	}
	if _, _, ok := m.Snapshot("first"); ok {
		t.Fatal("first request wrote")
	}
	if _, _, ok := m.Snapshot("second"); ok {
		t.Fatal("cross-request ticket wrote")
	}
}

func TestMemoryEditRejectsTicketFromAnotherRealExecutorRequest(t *testing.T) {
	m := fixture.NewMemory()
	version := m.SeedFile("first", []byte("before"))
	m.SeedFile("second", []byte("before"))
	ref := m.RegisterPatch(fixture.Patch{Old: "before", New: "after"})
	var first agent.AuthorizedFileEdit
	p := &filesProbe{Memory: m, edit: func(_ context.Context, r agent.AuthorizedFileEdit) (agent.FileEffect, error) {
		first = r
		return agent.FileEffect{SideEffect: "none"}, product.NewError(product.CodeResourceUnavailable, "test pre-start stop")
	}}
	run(t, m, "edit_file", map[string]any{"path": "first", "patchRef": ref, "expectedVersion": version}, p, m)
	p.edit = func(ctx context.Context, r agent.AuthorizedFileEdit) (agent.FileEffect, error) {
		r.Authorization.Ticket = first.Authorization.Ticket
		effect, err := m.Edit(ctx, r)
		code(t, err, product.CodePermissionDenied)
		return effect, err
	}
	out := run(t, m, "edit_file", map[string]any{"path": "second", "patchRef": ref, "expectedVersion": version}, p, m)
	if out.Executed || out.SideEffect != "none" || m.Calls("edit") != 1 {
		t.Fatalf("out=%+v calls=%d", out, m.Calls("edit"))
	}
	for _, path := range []string{"first", "second"} {
		data, current, ok := m.Snapshot(path)
		if !ok || string(data) != "before" || current != version {
			t.Fatalf("cross-request ticket changed %s", path)
		}
	}
}

func TestUnsupportedOperationsAndMissingReferences(t *testing.T) {
	m := fixture.NewMemory()
	_, err := m.List(t.Context(), agent.ListRequest{})
	code(t, err, product.CodeUnsupportedCapability)
	_, err = m.Search(t.Context(), agent.SearchRequest{})
	code(t, err, product.CodeUnsupportedCapability)
	_, err = m.Read(t.Context(), agent.ReadRequest{Identity: "missing"})
	code(t, err, product.CodeNotFound)
	_, err = m.Write(t.Context(), agent.AuthorizedFileWrite{})
	code(t, err, product.CodePermissionDenied)
	_, err = m.Edit(t.Context(), agent.AuthorizedFileEdit{})
	code(t, err, product.CodePermissionDenied)
}
