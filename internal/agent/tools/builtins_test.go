package tools

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
)

func TestBuiltinReadFileUsesControlledOperationsAndArtifact(t *testing.T) {
	files := &builtinFileProbe{read: agent.ReadResult{ContentRef: "artifact:read", Version: "v1"}}
	artifacts := &builtinArtifactProbe{content: "你好🙂\nsecond\n"}
	defs := NewBuiltinDefinitions(BuiltinOptions{Files: files, Artifacts: artifacts})
	def := builtinByName(t, defs, "read_file")
	sink := &recordSink{found: true, rec: builtinAccepted(`{"path":"src/main.go","offset":0,"limit":2000}`, "read_file", "provider-read")}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Files: files, Artifacts: artifacts}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "provider-read", "read_file", `{"path":"src/main.go","offset":0,"limit":2000}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "succeeded" || decodeReadPage(t, out).Content != "你好🙂\nsecond\n" || files.reads.Load() != 1 || artifacts.opens.Load() != 1 {
		t.Fatalf("unexpected controlled read: out=%+v reads=%d opens=%d", out, files.reads.Load(), artifacts.opens.Load())
	}
	if got := files.lastRead; got.Identity != "src/main.go" || got.Offset != 0 || got.Limit != 2000 {
		t.Fatalf("read request was not frozen as expected: %+v", got)
	}
}

func TestBuiltinExecuteUsesControlledProcessAndPreservesArguments(t *testing.T) {
	process := &builtinProcessProbe{}
	defs := NewBuiltinDefinitions(BuiltinOptions{Process: process})
	def := builtinByName(t, defs, "execute")
	sink := &recordSink{found: true, rec: builtinAccepted(`{"argv":["go","test","./..."],"cwd":"workspace/sub dir","shell":"native"}`, "execute", "provider-execute")}
	exec, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()),
		WithOperations(Operations{Process: process}), WithResourceScheduler(NewResourceScheduler()), WithResourceDomain("memory", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "provider-execute", "execute", `{"argv":["go","test","./..."],"cwd":"workspace/sub dir","shell":"native"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "succeeded" || !out.Executed || process.calls.Load() != 1 {
		t.Fatalf("unexpected controlled execute: out=%+v calls=%d", out, process.calls.Load())
	}
	if got := process.last; strings.Join(got.Argv, " ") != "go test ./..." || got.Cwd != "workspace/sub dir" || got.Shell != "native" {
		t.Fatalf("process request lost explicit command fields: %+v", got)
	}
}

func builtinByName(t *testing.T, defs []Definition, name string) Definition {
	t.Helper()
	for _, def := range defs {
		if def.Name == name {
			return def
		}
	}
	t.Fatalf("builtin %q was not registered", name)
	return Definition{}
}

type builtinFileProbe struct {
	read     agent.ReadResult
	reads    atomic.Int32
	lastRead agent.ReadRequest
}

func (*builtinFileProbe) List(context.Context, agent.ListRequest) (agent.ListResult, error) {
	return agent.ListResult{}, nil
}
func (f *builtinFileProbe) Read(ctx context.Context, req agent.ReadRequest) (agent.ReadResult, error) {
	f.reads.Add(1)
	f.lastRead = req
	return f.read, nil
}
func (*builtinFileProbe) Write(context.Context, agent.AuthorizedFileWrite) (agent.FileEffect, error) {
	return agent.FileEffect{}, nil
}
func (*builtinFileProbe) Edit(context.Context, agent.AuthorizedFileEdit) (agent.FileEffect, error) {
	return agent.FileEffect{}, nil
}
func (*builtinFileProbe) Search(context.Context, agent.SearchRequest) (agent.SearchResult, error) {
	return agent.SearchResult{}, nil
}

type builtinArtifactProbe struct {
	content string
	opens   atomic.Int32
}

func (*builtinArtifactProbe) Save(context.Context, agent.ArtifactInput) (agent.ArtifactRef, error) {
	return agent.ArtifactRef{}, nil
}
func (a *builtinArtifactProbe) Open(ctx context.Context, req agent.ArtifactRead) (io.ReadCloser, error) {
	if req.Ref.ID == "" {
		return nil, context.Canceled
	}
	a.opens.Add(1)
	return io.NopCloser(strings.NewReader(a.content)), nil
}

type builtinProcessProbe struct {
	calls atomic.Int32
	last  agent.AuthorizedProcess
}

func (p *builtinProcessProbe) Execute(ctx context.Context, req agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := req.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	p.calls.Add(1)
	p.last = req
	return agent.ProcessObservation{Started: true, Terminated: true, ExitCode: 0, Content: "ok", SideEffect: "none"}, nil
}
func (*builtinProcessProbe) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}

func TestBuiltinDefinitionSchemasAreJSONObjects(t *testing.T) {
	for _, def := range NewBuiltinDefinitions(BuiltinOptions{}) {
		var schema map[string]any
		if err := json.Unmarshal(def.Schema, &schema); err != nil {
			t.Fatalf("%s schema: %v", def.Name, err)
		}
		if schema["type"] != "object" {
			t.Fatalf("%s schema is not an object: %s", def.Name, def.Schema)
		}
	}
}

func builtinAccepted(args, name, provider string) agent.ToolRecord {
	return agent.ToolRecord{Call: agent.FrozenCall{CallID: provider, ProviderCallID: provider, Name: name, Arguments: args, Generation: "gen"}}
}
