package consumer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ww1489/seasprak/sdk"
)

type consumerReadPage struct {
	Content, Version, Mode, Encoding string
	ByteOffset, ByteEnd, Lines       int64
	Truncated, PartialLine           bool
	NextRead                         json.RawMessage
}

type consumerReadSnapshot struct {
	request          sdk.ReadRequest
	content, version string
}

// A migrated injected backend exposes whole immutable snapshots. All product
// types, including execution authorization, come from the sole SDK import.
type consumerReadBackend struct {
	publicFiles
	mu               sync.Mutex
	content, version string
	requests         []sdk.ReadRequest
	opens            int
	snapshots        map[string]consumerReadSnapshot
}

func (*consumerReadBackend) ExecutionCapabilities(context.Context) (sdk.BackendCapabilities, error) {
	return sdk.BackendCapabilities{BackendID: "consumer-read", Version: "snapshot-v1", EnvironmentID: "consumer-memory", SupportedModes: []string{"workspace-write"}, Enforcement: "full", RuntimeDataWriteProtected: true}, nil
}

func (b *consumerReadBackend) Read(ctx context.Context, r sdk.ReadRequest) (sdk.ReadResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, r)
	if err := ctx.Err(); err != nil {
		return sdk.ReadResult{}, err
	}
	if r.Identity != "file" || (r.Mode != "lines" && r.Mode != "bytes") {
		return sdk.ReadResult{}, sdk.NewError(sdk.CodeInvalidArgument, "unsupported snapshot request")
	}
	if r.Version != "" && r.Version != b.version {
		return sdk.ReadResult{}, sdk.NewError(sdk.CodeStateConflict, "snapshot version changed")
	}
	ref := fmt.Sprintf("snapshot-%d", len(b.requests))
	b.snapshots[ref] = consumerReadSnapshot{request: r, content: b.content, version: b.version}
	return sdk.ReadResult{ContentRef: ref, Version: b.version}, nil
}

func (*consumerReadBackend) Save(context.Context, sdk.ArtifactInput) (sdk.ArtifactRef, error) {
	return sdk.ArtifactRef{}, sdk.NewError(sdk.CodeUnsupportedCapability, "read-only fixture")
}

func (b *consumerReadBackend) Open(ctx context.Context, r sdk.ArtifactRead) (io.ReadCloser, error) {
	b.mu.Lock()
	b.opens++
	snapshot, found := b.snapshots[r.Ref.ID]
	b.mu.Unlock()
	f := r.Authorization.Frozen
	var args struct {
		Path, Version                        string
		Offset, Limit, ByteOffset, ByteLimit *int64
	}
	if !found || r.Offset != 0 || r.Limit != 0 || f.BackendID != "file-operations" || f.Tool != "read_file" || json.Unmarshal(f.FinalArguments, &args) != nil {
		return nil, sdk.NewError(sdk.CodePermissionDenied, "snapshot binding rejected")
	}
	want := sdk.ReadRequest{Identity: args.Path, Version: args.Version, Mode: "lines"}
	offset, limit := args.Offset, args.Limit
	if args.ByteOffset != nil || args.ByteLimit != nil {
		if offset != nil || limit != nil {
			return nil, sdk.NewError(sdk.CodePermissionDenied, "mixed snapshot range")
		}
		want.Mode, offset, limit = "bytes", args.ByteOffset, args.ByteLimit
	}
	if offset != nil {
		want.Offset = *offset
	}
	if limit != nil {
		want.Limit = *limit
	}
	if want != snapshot.request || len(f.Resources) != 1 || f.Resources[0].Identity != "path:"+want.Identity || f.Resources[0].ExpectedVersion != want.Version || (want.Version != "" && want.Version != snapshot.version) {
		return nil, sdk.NewError(sdk.CodePermissionDenied, "snapshot request changed")
	}
	if err := r.Authorization.Validate(ctx); err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(snapshot.content)), nil
}

func TestSDKConsumerReadSnapshotThroughSession(t *testing.T) {
	for _, mode := range []string{"lines", "bytes"} {
		for _, conflict := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/conflict-%t", mode, conflict), func(t *testing.T) {
				const source = "中🙂\r\n尾"
				backend := &consumerReadBackend{content: source, version: "v1", snapshots: map[string]consumerReadSnapshot{}}
				args := `{"path":"file","limit":1}`
				if mode == "bytes" {
					args = `{"path":"file","byteLimit":7}`
				}
				model := &consumerReadModel{initial: args}
				if conflict {
					model.afterFirst = func() {
						backend.mu.Lock()
						backend.content, backend.version = "changed", "v2"
						backend.mu.Unlock()
					}
				}
				s, err := sdk.CreateAgentSession(t.Context(), sdk.SessionOptions{
					SessionID: "consumer-read", Workspace: t.TempDir(), StateRoot: "memory", Profile: sdk.ProfileMemory,
					Model: model, Tools: sdk.NewBuiltinDefinitions(sdk.BuiltinOptions{}), Operations: sdk.Operations{Files: backend, Artifacts: backend},
				})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close(context.Background())
				input, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"read the file"}`)})
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.NewTimer(10 * time.Second)
				defer deadline.Stop()
				var view sdk.Snapshot
				for {
					view, err = s.Snapshot(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if trace := view.Traces[input.TraceID]; trace != nil && trace.Settled {
						if trace.State != "completed" {
							t.Fatalf("read trace state=%s error=%s", trace.State, trace.Error)
						}
						break
					}
					select {
					case <-deadline.C:
						t.Fatal("read trace did not settle")
					default:
						runtime.Gosched()
					}
				}
				model.mu.Lock()
				defer model.mu.Unlock()
				backend.mu.Lock()
				defer backend.mu.Unlock()
				wantPages, wantOpens := 2, 2
				if conflict {
					wantPages, wantOpens = 1, 1
				}
				if model.problem != "" || model.calls != 3 || model.failedResult != conflict || len(model.pages) != wantPages || len(backend.requests) != 2 || backend.opens != wantOpens || len(view.Calls) != 2 || view.Traces[input.TraceID].Usage.ToolExecutions != 2 {
					t.Fatalf("read pipeline counts: model=%d pages=%d reads=%d opens=%d calls=%d problem=%s", model.calls, len(model.pages), len(backend.requests), backend.opens, len(view.Calls), model.problem)
				}
				var joined strings.Builder
				var end int64
				for _, page := range model.pages {
					if page.Version != "v1" || page.Mode != mode || page.Encoding != "utf-8" || !utf8.ValidString(page.Content) || page.ByteOffset != end || page.ByteEnd-page.ByteOffset != int64(len(page.Content)) {
						t.Fatal("model received incorrect structured snapshot metadata")
					}
					joined.WriteString(page.Content)
					end = page.ByteEnd
				}
				if !conflict && (joined.String() != source || model.pages[1].Truncated || string(model.pages[1].NextRead) != "null") {
					t.Fatal("model-driven continuation lost or repeated source content")
				}
				first := model.pages[0]
				var next map[string]any
				if json.Unmarshal(first.NextRead, &next) != nil || !first.Truncated || next["version"] != "v1" || next["path"] != "file" {
					t.Fatal("missing version-bound continuation")
				}
				wantOffset, wantLimit := int64(1), int64(1)
				if mode == "bytes" {
					wantOffset, wantLimit = first.ByteEnd, 7
					if next["byteOffset"] != float64(first.ByteEnd) {
						t.Fatal("wrong byte continuation")
					}
				} else if next["offset"] != float64(1) {
					t.Fatal("wrong line continuation")
				}
				if backend.requests[0].Version != "" || backend.requests[1] != (sdk.ReadRequest{Identity: "file", Version: "v1", Mode: mode, Offset: wantOffset, Limit: wantLimit}) {
					t.Fatal("public backend did not receive the exact continuation binding")
				}
				failed := 0
				for _, call := range view.Calls {
					obs := call.Observation
					if obs == nil {
						t.Fatal("read observation missing")
					}
					if obs.Status == "failed" {
						failed++
						if !conflict || obs.ExecutionError != sdk.CodeStateConflict || obs.Executed {
							t.Fatal("wrong version-conflict observation")
						}
					} else if obs.Status != "succeeded" || !obs.Executed || !json.Valid([]byte(obs.Content)) {
						t.Fatal("read observation is not complete successful JSON")
					}
				}
				if failed != 2-wantPages {
					t.Fatal("unexpected number of failed reads")
				}
			})
		}
	}
}

var _ sdk.FileOperations = (*consumerReadBackend)(nil)
var _ sdk.ArtifactStore = (*consumerReadBackend)(nil)
var _ sdk.BackendCapabilityReporter = (*consumerReadBackend)(nil)
