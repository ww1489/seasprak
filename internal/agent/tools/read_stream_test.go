package tools

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

type readStreamProbe struct {
	*fixture.Memory
	bytes  int
	closed bool
}

func (p *readStreamProbe) Open(ctx context.Context, r agent.ArtifactRead) (io.ReadCloser, error) {
	if r.Offset != 0 || r.Limit != 0 {
		return nil, product.NewError(product.CodeInvalidArgument, "snapshot was sliced twice")
	}
	reader, err := p.Memory.Open(ctx, r)
	if err != nil {
		return nil, err
	}
	return &readCountingStream{ReadCloser: reader, probe: p}, nil
}

type readCountingStream struct {
	io.ReadCloser
	probe *readStreamProbe
}

func (r *readCountingStream) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	r.probe.bytes += n
	return n, err
}
func (r *readCountingStream) Close() error { r.probe.closed = true; return r.ReadCloser.Close() }

func TestReadRangesSkipLongLineWithoutReadingEntireSnapshot(t *testing.T) {
	m := fixture.NewMemory()
	skipped := strings.Repeat("中", 60000) + "\r\n"
	m.SeedFile("file", []byte(skipped+"selected🙂\r\n"+strings.Repeat("unrequested\n", 100000)))
	p := &readStreamProbe{Memory: m}
	out, _ := runReadRange(t, m, p, `{"path":"file","offset":1,"limit":1}`)
	page := decodeReadPage(t, out)
	if page.Content != "selected🙂\r\n" || page.ByteOffset != int64(len(skipped)) || page.Lines != 1 || p.bytes > len(skipped)+8192 || !p.closed || m.Calls("read") != 1 || m.Calls("open") != 1 {
		t.Fatalf("read did not stream a single bounded range: bytes=%d", p.bytes)
	}
}

func TestReadRangesHardLimitsJSONEscapingAndEmptyRanges(t *testing.T) {
	for _, tc := range []struct {
		source, args string
		bytes        int
		truncated    bool
	}{
		{strings.Repeat("\"\\\t\r\n", 3000), `{"path":"file","limit":99999}`, 10000, true},
		{strings.Repeat("\"\\", 40000), `{"path":"file","byteLimit":99999}`, 50 << 10, true},
		{"", `{"path":"file"}`, 0, false},
		{"one\n", `{"path":"file","offset":999}`, 0, false},
		{"one\n", `{"path":"file","byteOffset":999}`, 0, false},
		{"中🙂", `{"path":"file","byteOffset":3,"byteLimit":4}`, 4, false},
	} {
		m := fixture.NewMemory()
		m.SeedFile("file", []byte(tc.source))
		out, _ := runReadRange(t, m, m, tc.args)
		page := decodeReadPage(t, out)
		if len(page.Content) != tc.bytes || page.Truncated != tc.truncated || !json.Valid([]byte(out.ModelContent())) {
			t.Fatalf("invalid bounded JSON: size=%d truncated=%v", len(page.Content), page.Truncated)
		}
	}
}

func TestReadRangesByteModeCapsLinesAndContinues(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		m := fixture.NewMemory()
		first := strings.Repeat("中🙂"+newline, 2000)
		tail := "tail" + newline + "末行🙂"
		source := first + tail
		version := m.SeedFile("file", []byte(source))
		out, _ := runReadRange(t, m, m, `{"path":"file","byteLimit":51200}`)
		page := decodeReadPage(t, out)
		if page.Content != first || page.Lines != 2000 || page.ByteOffset != 0 || page.ByteEnd != int64(len(first)) || !page.Truncated || page.Reason != "line_limit" || page.PartialLine || !json.Valid([]byte(out.ModelContent())) {
			t.Fatalf("byte page ignored line limit: lines=%d end=%d truncated=%v reason=%s", page.Lines, page.ByteEnd, page.Truncated, page.Reason)
		}
		var next struct {
			Path, Version         string
			ByteOffset, ByteLimit int64
			Offset, Limit         *int64
		}
		if err := json.Unmarshal(page.NextRead, &next); err != nil || next.Path != "file" || next.Version != version || next.ByteOffset != int64(len(first)) || next.ByteLimit != 51200 || next.Offset != nil || next.Limit != nil {
			t.Fatal("line-limited byte page lost its exact versioned continuation")
		}
		out, _ = runReadRange(t, m, m, string(page.NextRead))
		last := decodeReadPage(t, out)
		if page.Content+last.Content != source || last.Version != version || last.ByteOffset != page.ByteEnd || last.ByteEnd != int64(len(source)) || last.Lines != 2 || last.Truncated || last.PartialLine || string(last.NextRead) != "null" || m.Calls("read") != 2 || m.Calls("open") != 2 {
			t.Fatalf("byte continuation lost content or line metadata: lines=%d reads=%d opens=%d", last.Lines, m.Calls("read"), m.Calls("open"))
		}
	}
}

func TestReadRangesByteModeCountsFinalUnterminatedLine(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		lines        int64
	}{
		{"empty", "", 0},
		{"unterminated", "末行🙂", 1},
		{"terminated", "line\n", 1},
		{"empty-line", "\n", 1},
		{"exact-limit-unterminated", strings.Repeat("x\n", 1999) + "末行🙂", 2000},
		{"exact-limit-terminated", strings.Repeat("x\n", 2000), 2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixture.NewMemory()
			m.SeedFile("file", []byte(tc.source))
			out, _ := runReadRange(t, m, m, `{"path":"file","byteLimit":51200}`)
			page := decodeReadPage(t, out)
			if page.Content != tc.source || page.Lines != tc.lines || page.Truncated || page.PartialLine || page.ByteEnd != int64(len(tc.source)) || string(page.NextRead) != "null" || m.Calls("read") != 1 || m.Calls("open") != 1 {
				t.Fatalf("incorrect final line metadata: lines=%d want=%d truncated=%v", page.Lines, tc.lines, page.Truncated)
			}
		})
	}
}

func TestReadRangesSmallBytePagesAndByteVersionConflict(t *testing.T) {
	for _, limit := range []int{4, 5, 6, 7, 8} {
		m := fixture.NewMemory()
		source := "中🙂文\r\n🙂中"
		m.SeedFile("file", []byte(source))
		raw, _ := json.Marshal(map[string]any{"path": "file", "byteLimit": limit})
		args := string(raw)
		var joined strings.Builder
		for i := 0; i < 20; i++ {
			out, _ := runReadRange(t, m, m, args)
			page := decodeReadPage(t, out)
			joined.WriteString(page.Content)
			if string(page.NextRead) == "null" || len(page.NextRead) == 0 {
				break
			}
			args = string(page.NextRead)
		}
		if joined.String() != source {
			t.Fatalf("limit %d lost or repeated characters", limit)
		}
	}
	m := fixture.NewMemory()
	m.SeedFile("file", []byte("中🙂"))
	out, _ := runReadRange(t, m, m, `{"path":"file","byteLimit":4}`)
	page := decodeReadPage(t, out)
	m.SeedFile("file", []byte("文🙂"))
	out, _ = runReadRange(t, m, m, string(page.NextRead))
	if out.ExecutionError != product.CodeStateConflict || out.Status != "failed" || m.Calls("read") != 2 || m.Calls("open") != 1 {
		t.Fatal("byte continuation accepted a new version")
	}
}
