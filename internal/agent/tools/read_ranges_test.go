package tools

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

type readPageProbe struct {
	Content, Version, Mode, Reason, Encoding string
	ByteOffset, ByteEnd, Lines               int64
	Truncated, PartialLine                   bool
	NextRead                                 json.RawMessage
}

func runReadRange(t *testing.T, files agent.FileOperations, artifacts agent.ArtifactStore, args string) (Outcome, *recordSink) {
	t.Helper()
	sink := &recordSink{found: true, rec: builtinAccepted(args, "read_file", "read-range")}
	e, err := NewExecutor("gen", []Definition{builtinByName(t, NewBuiltinDefinitions(BuiltinOptions{}), "read_file")}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Files: files, Artifacts: artifacts}), WithResourceScheduler(NewResourceScheduler()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "read-range", "read_file", args)
	if err != nil {
		t.Fatal(err)
	}
	return out, sink
}

func decodeReadPage(t *testing.T, out Outcome) readPageProbe {
	t.Helper()
	var page readPageProbe
	if out.Status != "succeeded" || !out.Executed || json.Unmarshal([]byte(out.Content), &page) != nil {
		t.Fatalf("read must return successful complete JSON: status=%s executed=%v JSON=%v", out.Status, out.Executed, json.Valid([]byte(out.Content)))
	}
	if page.Version == "" || page.Encoding != "utf-8" || !utf8.ValidString(page.Content) {
		t.Fatal("missing version or invalid encoding")
	}
	return page
}

func TestReadRangesRejectMixedModesBeforeBackend(t *testing.T) {
	for _, args := range []string{
		`{"path":"file","offset":0,"byteOffset":0}`,
		`{"path":"file","limit":1,"byteLimit":4}`,
		`{"path":"file","offset":0,"byteLimit":4}`,
		`{"path":"file","limit":1,"byteOffset":0}`,
	} {
		m := fixture.NewMemory()
		m.SeedFile("file", []byte("body"))
		out, _ := runReadRange(t, m, m, args)
		if out.Status != "failed" || out.Executed || m.Calls("read") != 0 || m.Calls("open") != 0 {
			t.Fatalf("mixed ranges executed: %+v", out)
		}
	}
	// A valid byte range must reach the backend: rejecting all byte fields is not support.
	m := fixture.NewMemory()
	m.SeedFile("file", []byte("body"))
	out, _ := runReadRange(t, m, m, `{"path":"file","byteOffset":0,"byteLimit":4}`)
	if out.Status != "succeeded" || m.Calls("read") != 1 || m.Calls("open") != 1 {
		t.Fatalf("valid byte range rejected: %+v", out)
	}
}

func TestReadRangesLinesPreserveSourceAndContinuation(t *testing.T) {
	m := fixture.NewMemory()
	source := "头🙂\r\n第二行\r\n\r\nend"
	version := m.SeedFile("file", []byte(source))
	args := `{"path":"file","limit":1}`
	var joined strings.Builder
	var end int64
	for i := 0; i < 5; i++ {
		out, sink := runReadRange(t, m, m, args)
		page := decodeReadPage(t, out)
		if page.Version != version || page.Mode != "lines" || page.PartialLine || page.Lines != 1 || page.ByteOffset != end {
			t.Fatalf("wrong line metadata: %+v", page)
		}
		joined.WriteString(page.Content)
		end = page.ByteEnd
		// The durable observation contains the exact structured result, not a second projection.
		found := false
		for _, fact := range sink.facts {
			if fact.Kind == "tool_observation" {
				var r agent.ToolRecord
				if err := json.Unmarshal(fact.Payload, &r); err != nil {
					t.Fatal(err)
				}
				found = r.Observation != nil && r.Observation.Content == out.Content
			}
		}
		if !found {
			t.Fatal("read JSON missing from observation")
		}
		if string(page.NextRead) == "null" || len(page.NextRead) == 0 {
			break
		}
		if !page.Truncated || page.Reason != "line_limit" {
			t.Fatalf("wrong continuation: %+v", page)
		}
		args = string(page.NextRead)
	}
	if joined.String() != source || end != int64(len(source)) || m.Calls("read") != 4 || m.Calls("open") != 4 {
		t.Fatalf("source lost or repeated: %q", joined.String())
	}
}

func TestReadRangesDefaultLimitsAndLongFirstLine(t *testing.T) {
	for _, tc := range []struct {
		name, source, reason string
		lines                int64
		size                 int
	}{
		{"lines", strings.Repeat("行\r\n", 2001), "line_limit", 2000, 10000},
		{"bytes", strings.Repeat(strings.Repeat("x", 1023)+"\n", 51), "byte_limit", 50, 50 << 10},
		{"long", strings.Repeat("中🙂", 9000) + "\r\n", "line_too_long", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixture.NewMemory()
			m.SeedFile("file", []byte(tc.source))
			out, _ := runReadRange(t, m, m, `{"path":"file"}`)
			page := decodeReadPage(t, out)
			if !page.Truncated || page.Reason != tc.reason || page.Lines != tc.lines || len(page.Content) != tc.size || page.PartialLine || len(page.NextRead) == 0 {
				t.Fatalf("wrong bounded read: bytes=%d page=%+v", len(page.Content), page)
			}
			if tc.name == "long" {
				var next map[string]any
				_ = json.Unmarshal(page.NextRead, &next)
				if next["byteOffset"] != float64(0) || next["byteLimit"] != float64(50<<10) || next["version"] != page.Version {
					t.Fatal("long line has no usable fragment request")
				}
			}
		})
	}
}

func TestReadRangesBytePagesExactlyReassembleUnicode(t *testing.T) {
	for _, source := range []string{"中🙂文\r\nsecond\n", strings.Repeat("中🙂", 16000) + "\r\n", strings.Repeat("English", 16000)} {
		m := fixture.NewMemory()
		m.SeedFile("file", []byte(source))
		args := `{"path":"file","byteOffset":0,"byteLimit":51199}`
		var joined strings.Builder
		var end int64
		for i := 0; i < 8; i++ {
			out, _ := runReadRange(t, m, m, args)
			page := decodeReadPage(t, out)
			if page.Mode != "bytes" || page.ByteOffset != end || page.ByteEnd-page.ByteOffset != int64(len(page.Content)) || len(page.Content) > 51199 {
				t.Fatal("incorrect byte range")
			}
			joined.WriteString(page.Content)
			end = page.ByteEnd
			if string(page.NextRead) == "null" || len(page.NextRead) == 0 {
				break
			}
			if !page.Truncated || !page.PartialLine {
				t.Fatal("byte fragment not identified")
			}
			args = string(page.NextRead)
		}
		if joined.String() != source || end != int64(len(source)) {
			t.Fatal("UTF-8 pagination lost or repeated bytes")
		}
	}
}

func TestReadRangesVersionConflictAndSourceEncoding(t *testing.T) {
	m := fixture.NewMemory()
	m.SeedFile("file", []byte("first\nsecond\n"))
	out, _ := runReadRange(t, m, m, `{"path":"file","limit":1}`)
	page := decodeReadPage(t, out)
	m.SeedFile("file", []byte("changed\nsecond\n"))
	out, _ = runReadRange(t, m, m, string(page.NextRead))
	if out.Status != "failed" || out.ExecutionError != product.CodeStateConflict || m.Calls("read") != 2 || m.Calls("open") != 1 {
		t.Fatalf("stale continuation accepted: %+v", out)
	}
	for _, tc := range []struct{ source, args, reason string }{
		{string([]byte{0xff, '\n'}), `{"path":"file"}`, "invalid_utf8"},
		{string([]byte{0x80}), `{"path":"file","byteLimit":4}`, "invalid_utf8"},
		{string([]byte{0xff}) + strings.Repeat("x", 60000), `{"path":"file"}`, "invalid_utf8"},
		{"中🙂", `{"path":"file","byteOffset":1,"byteLimit":4}`, "utf8_boundary"},
		{"🙂", `{"path":"file","byteLimit":1}`, "byte_limit_too_small"},
	} {
		m = fixture.NewMemory()
		m.SeedFile("file", []byte(tc.source))
		out, _ = runReadRange(t, m, m, tc.args)
		if out.Status != "failed" || out.ExecutionError != product.CodeInvalidArgument || !strings.Contains(out.Content, tc.reason) {
			t.Fatalf("encoding error not explicit: %+v", out)
		}
	}
}

type unreadableReadArtifact struct{ calls int }

func (*unreadableReadArtifact) Save(context.Context, agent.ArtifactInput) (agent.ArtifactRef, error) {
	return agent.ArtifactRef{}, nil
}
func (p *unreadableReadArtifact) Open(context.Context, agent.ArtifactRead) (io.ReadCloser, error) {
	p.calls++
	return nil, product.NewError(product.CodeNotFound, "missing")
}

func TestReadRangesRequiresReadableArtifactReference(t *testing.T) {
	for _, kind := range []string{"no-store", "missing", "empty-ref"} {
		t.Run(kind, func(t *testing.T) {
			files := &builtinFileProbe{read: agent.ReadResult{ContentRef: "opaque:not-body", Version: "v1"}}
			var artifacts agent.ArtifactStore
			if kind == "missing" {
				artifacts = &unreadableReadArtifact{}
			}
			if kind == "empty-ref" {
				files.read.ContentRef = ""
				artifacts = &builtinArtifactProbe{content: "body"}
			}
			out, _ := runReadRange(t, files, artifacts, `{"path":"file"}`)
			if out.Status != "failed" || out.Content == "opaque:not-body" || out.ExecutionError == "" || out.Executed {
				t.Fatalf("unreadable ref reported success: %+v", out)
			}
		})
	}
}
