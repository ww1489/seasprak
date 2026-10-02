package codeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	einofs "github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

type sessionDiscoveryPage struct {
	Entries []agent.FileEntry   `json:"entries"`
	Matches []agent.SearchMatch `json:"matches"`
	Files   []string            `json:"files"`
	Counts  []struct {
		Identity string
		Count    int
	} `json:"counts"`
	Returned  int      `json:"returned"`
	Truncated bool     `json:"truncated"`
	Reasons   []string `json:"truncationReasons"`
	Hint      string   `json:"hint"`
}

// The script still comes from testkit.FakeModel. This wrapper inspects exactly
// the next model request and may bind the scripted next call to a returned path.
// It does not invoke tools or synthesize backend results.
type sessionDiscoveryModel struct {
	fake     *testkit.FakeModel
	mu       sync.Mutex
	checks   []func([]*schema.AgenticMessage) (string, error)
	observed int
	problem  error
}

func (m *sessionDiscoveryModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	index := m.fake.Calls()
	var args string
	if index < len(m.checks) && m.checks[index] != nil {
		var err error
		args, err = m.checks[index](in)
		if err != nil {
			m.problem = err
			return nil, err
		}
		m.observed++
	}
	out, err := m.fake.Generate(ctx, in, opts...)
	if err == nil && args != "" {
		for _, block := range out.ContentBlocks {
			if block.FunctionToolCall != nil {
				block.FunctionToolCall.Arguments = args
			}
		}
	}
	return out, err
}
func (m *sessionDiscoveryModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	out, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{out}), nil
}
func discoveryModelPage(in []*schema.AgenticMessage, id string) (sessionDiscoveryPage, error) {
	var p sessionDiscoveryPage
	found := 0
	for _, message := range in {
		for _, block := range message.ContentBlocks {
			r := block.FunctionToolResult
			if r == nil || r.CallID != id {
				continue
			}
			found++
			var text strings.Builder
			for _, part := range r.Content {
				if part.Text != nil {
					text.WriteString(part.Text.Text)
				}
			}
			raw := text.String()
			if !utf8.ValidString(raw) || !json.Valid([]byte(raw)) {
				return p, fmt.Errorf("%s: invalid model JSON/UTF-8", id)
			}
			// Truncated observations use the SDK's existing output projection envelope.
			var envelope struct {
				Content   *string `json:"content"`
				Truncated bool    `json:"truncated"`
			}
			if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
				return p, err
			}
			if envelope.Content != nil {
				if !envelope.Truncated {
					return p, fmt.Errorf("%s: missing envelope truncation", id)
				}
				raw = *envelope.Content
			}
			if len(raw) > 50*1024 || !utf8.ValidString(raw) {
				return p, fmt.Errorf("%s: invalid bounded discovery payload", id)
			}
			if err := json.Unmarshal([]byte(raw), &p); err != nil {
				return p, err
			}
			if p.Truncated && (p.Hint == "" || len(p.Reasons) == 0) {
				return p, fmt.Errorf("%s: missing incomplete-result explanation", id)
			}
		}
	}
	if found != 1 {
		return p, fmt.Errorf("%s: expected one paired result, got %d", id, found)
	}
	return p, nil
}
func discoveryScript(name, id, args string) testkit.Step {
	return testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: id, Name: name, Arguments: args}}}
}
func runSessionDiscovery(t *testing.T, files *fixture.Memory, m *sessionDiscoveryModel, defs []tools.Definition, deny bool) {
	t.Helper()
	opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: m, Tools: defs, Operations: tools.Operations{Files: files, Artifacts: files}, ResourceScheduler: tools.NewResourceScheduler()}
	if deny {
		opts.Policy = &agent.ResolvedPolicy{ApprovalPolicy: "never", SandboxMode: "workspace-write"}
	}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"discover files using controlled tools"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { tr := s.rt.manager.View().Traces[input.TraceID]; return tr != nil && tr.Settled })
	view := s.rt.manager.View()
	tr := view.Traces[input.TraceID]
	m.mu.Lock()
	problem, observed := m.problem, m.observed
	m.mu.Unlock()
	wantState := "completed"
	wantTools := m.fake.Calls() - 1
	if deny {
		wantState = "failed"
		wantTools = 0
	}
	if problem != nil || tr.State != wantState || tr.Usage.ToolExecutions != wantTools || (!deny && observed != wantTools) {
		t.Fatalf("trace=%+v observed=%d model=%d problem=%v", tr, observed, m.fake.Calls(), problem)
	}
	wantCalls := wantTools
	if deny {
		wantCalls = 1
		if !strings.Contains(tr.Error, product.CodePermissionDenied) {
			t.Fatalf("denial error=%s", tr.Error)
		}
	}
	if len(view.Calls) != wantCalls {
		t.Fatalf("accepted calls=%d want=%d", len(view.Calls), wantCalls)
	}
	for _, call := range view.Calls {
		obs := call.Observation
		status := "succeeded"
		if deny {
			status = "denied"
		}
		if obs == nil || obs.Status != status || obs.SideEffect != "none" || obs.Executed == deny || call.Claimed == deny {
			t.Fatalf("call=%+v", call)
		}
	}
	if deny {
		for _, ev := range view.Events {
			if ev.Type == "tool.started" {
				t.Fatal("denied file call started")
			}
		}
	}
}

func TestFileDiscoverySessionMultiTurnUsesReturnedPaths(t *testing.T) {
	files := fixture.NewMemory()
	files.SeedFile("root/z/last.go", []byte("needle"))
	files.SeedFile("root/a/020.go", []byte("needle"))
	files.SeedFile("root/a/010.go", []byte("needle\nneedle"))
	files.SeedFile("root/m/other.txt", []byte("other"))
	m := &sessionDiscoveryModel{fake: testkit.NewFake(
		discoveryScript("ls", "list", `{"root":"root","limit":2}`),
		discoveryScript("glob", "paths", `{}`), discoveryScript("grep", "content", `{}`), testkit.Step{Text: "done"})}
	m.checks = []func([]*schema.AgenticMessage) (string, error){nil,
		func(in []*schema.AgenticMessage) (string, error) {
			p, err := discoveryModelPage(in, "list")
			if err != nil {
				return "", err
			}
			if p.Returned != 2 || !p.Truncated || !slices.Contains(p.Reasons, "entry_limit") || len(p.Entries) != 2 || p.Entries[0].Identity != "root/a" || p.Entries[1].Identity != "root/m" {
				return "", fmt.Errorf("list=%+v", p)
			}
			b, _ := json.Marshal(map[string]any{"root": p.Entries[0].Identity, "pattern": "**/*.go", "limit": 1})
			return string(b), nil
		},
		func(in []*schema.AgenticMessage) (string, error) {
			p, err := discoveryModelPage(in, "paths")
			if err != nil {
				return "", err
			}
			if p.Returned != 1 || !p.Truncated || !slices.Contains(p.Reasons, "entry_limit") || len(p.Matches) != 1 || p.Matches[0].Identity != "root/a/010.go" {
				return "", fmt.Errorf("glob=%+v", p)
			}
			b, _ := json.Marshal(map[string]any{"root": p.Matches[0].Identity, "query": "needle", "output_mode": "count"})
			return string(b), nil
		},
		func(in []*schema.AgenticMessage) (string, error) {
			p, err := discoveryModelPage(in, "content")
			if err != nil {
				return "", err
			}
			if p.Returned != 1 || p.Truncated || len(p.Counts) != 1 || p.Counts[0].Identity != "root/a/010.go" || p.Counts[0].Count != 2 || len(p.Matches) != 0 || len(p.Files) != 0 {
				return "", fmt.Errorf("count mode=%+v", p)
			}
			return "", nil
		},
	}
	runSessionDiscovery(t, files, m, tools.NewBuiltinDefinitions(tools.BuiltinOptions{}), false)
	if m.fake.Calls() != 4 || files.Calls("list") != 1 || files.Calls("search") != 2 || files.Calls("read") != 0 {
		t.Fatalf("model=%d list=%d search=%d", m.fake.Calls(), files.Calls("list"), files.Calls("search"))
	}
}

func TestFileDiscoverySessionGrepModesAndLimits(t *testing.T) {
	for _, mode := range []string{"content", "files_with_matches", "count"} {
		t.Run(mode, func(t *testing.T) {
			files := fixture.NewMemory()
			for i := 39; i >= 0; i-- {
				files.SeedFile(fmt.Sprintf("root/%02d.go", i), []byte("needle"+strings.Repeat("界", 600)+"\nneedle"))
			}
			limit, offset := 1, 1
			if mode == "content" {
				limit, offset = 100, 0
			}
			args, _ := json.Marshal(map[string]any{"root": "root", "query": "needle", "output_mode": mode, "offset": offset, "head_limit": limit})
			m := &sessionDiscoveryModel{fake: testkit.NewFake(discoveryScript("grep", "grep", string(args)), testkit.Step{Text: "done"})}
			m.checks = []func([]*schema.AgenticMessage) (string, error){nil, func(in []*schema.AgenticMessage) (string, error) {
				p, err := discoveryModelPage(in, "grep")
				if err != nil {
					return "", err
				}
				if !p.Truncated {
					return "", fmt.Errorf("expected bounded %s", mode)
				}
				switch mode {
				case "content":
					if len(p.Matches) < 2 || p.Matches[0].Identity != "root/00.go" || p.Matches[0].Line != 1 || p.Matches[1].Line != 2 || len([]rune(p.Matches[0].Preview)) != 500 || !slices.Contains(p.Reasons, "line_limit") || !slices.Contains(p.Reasons, "byte_limit") || len(p.Counts) != 0 || len(p.Files) != 0 {
						return "", fmt.Errorf("content truncation=%+v", p.Reasons)
					}
				case "files_with_matches":
					if p.Returned != 1 || len(p.Files) != 1 || p.Files[0] != "root/01.go" || !slices.Contains(p.Reasons, "entry_limit") || len(p.Matches) != 0 || len(p.Counts) != 0 {
						return "", fmt.Errorf("files mode=%+v", p)
					}
				case "count":
					if p.Returned != 1 || len(p.Counts) != 1 || p.Counts[0].Identity != "root/01.go" || p.Counts[0].Count != 2 || !slices.Contains(p.Reasons, "entry_limit") || len(p.Files) != 0 || len(p.Matches) != 0 {
						return "", fmt.Errorf("count mode=%+v", p)
					}
				}
				return "", nil
			}}
			runSessionDiscovery(t, files, m, tools.NewBuiltinDefinitions(tools.BuiltinOptions{}), false)
			if m.fake.Calls() != 2 || files.Calls("search") != 1 || files.Calls("list") != 0 {
				t.Fatal("wrong model/backend counts")
			}
		})
	}
}

func TestFileDiscoverySessionPermissionDeniedZeroBackend(t *testing.T) {
	for _, name := range []string{"ls", "glob", "grep"} {
		t.Run(name, func(t *testing.T) {
			files := fixture.NewMemory()
			files.SeedFile("root/a.go", []byte("needle"))
			args := map[string]string{"ls": `{"root":"root"}`, "glob": `{"root":"root","pattern":"**/*.go"}`, "grep": `{"root":"root","query":"needle"}`}[name]
			def := builtinDefinitionForSession(t, name)
			def.Execution.RequestedGrantRef = "file-discovery-test-grant"
			m := &sessionDiscoveryModel{fake: testkit.NewFake(discoveryScript(name, "denied", args), testkit.Step{Text: "must not reach"})}
			runSessionDiscovery(t, files, m, []tools.Definition{def}, true)
			if m.fake.Calls() != 1 || files.Calls("search") != 0 || files.Calls("list") != 0 {
				t.Fatal("denial reached backend or next model")
			}
		})
	}
}

// A runnable dependency probe, not product certification. v0.9.21 normalizes
// with filepath.Clean, then checks root + "/" and slash-relative paths. Record
// the pinned version's actual platform behavior rather than infer portability.
func TestFileDiscoveryEinoMemoryPathProbe(t *testing.T) {
	b := einofs.NewInMemoryBackend()
	ctx := t.Context()
	for _, name := range []string{"root/a.go", "root/sub/b.go"} {
		if err := b.Write(ctx, &einofs.WriteRequest{FilePath: name, Content: "needle"}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := b.LsInfo(ctx, &einofs.LsInfoRequest{Path: "root"})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := b.GlobInfo(ctx, &einofs.GlobInfoRequest{Path: "root", Pattern: "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := b.GrepRaw(ctx, &einofs.GrepRequest{Path: "root", Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	want := 2
	if goruntime.GOOS == "windows" {
		want = 0
	}
	t.Logf("Eino v0.9.21 GOOS=%s clean=%q ls=%d glob=%d grep=%d", goruntime.GOOS, filepath.Clean("/root/a.go"), len(entries), len(paths), len(matches))
	if len(entries) != want || len(paths) != want || len(matches) != want {
		t.Fatalf("dependency path behavior changed: want=%d ls=%d glob=%d grep=%d", want, len(entries), len(paths), len(matches))
	}
	// Exact edit is portable because Write/Edit/Read share the same normalized key.
	if err := b.Edit(ctx, &einofs.EditRequest{FilePath: "root/a.go", OldString: "needle", NewString: "edited"}); err != nil {
		t.Fatal(err)
	}
	read, err := b.Read(ctx, &einofs.ReadRequest{FilePath: "root/a.go"})
	if err != nil || read.Content != "edited" {
		t.Fatalf("exact edit=%+v err=%v", read, err)
	}
}
