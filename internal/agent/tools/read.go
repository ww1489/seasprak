package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

type readPage struct {
	Content     string `json:"content"`
	Version     string `json:"version"`
	Mode        string `json:"mode"`
	Encoding    string `json:"encoding"`
	ByteOffset  int64  `json:"byteOffset"`
	ByteEnd     int64  `json:"byteEnd"`
	Lines       int64  `json:"lines"`
	Truncated   bool   `json:"truncated"`
	PartialLine bool   `json:"partialLine"`
	Reason      string `json:"reason"`
	NextRead    any    `json:"nextRead"`
}

const (
	readFailureContent   = "controlled file read failed"
	readCancelledContent = "controlled file read cancelled"
)

func readFailure(err error) Outcome {
	out := Outcome{Status: "failed", Content: readFailureContent, ExecutionError: product.CodeResourceUnavailable, SideEffect: "none"}
	var pe *product.Error
	if errors.As(err, &pe) && pe != nil {
		out.ExecutionError = pe.Code
	}
	if local, ok := err.(readDiagnostic); ok {
		out.ExecutionError, out.Content = local.code, local.Error()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		out.Status, out.Content = "cancelled", readCancelledContent
	}
	return out
}

// readDiagnostic is created only for this tool's literal local diagnostics.
// Backend product errors, even with identical messages, never get this type.
type readDiagnostic struct{ code, reason string }

func (d readDiagnostic) Error() string { return d.code + ": " + d.reason }

func localReadError(code, reason string) error {
	return readDiagnostic{code: code, reason: reason}
}

func (e *Executor) invokeRead(ctx context.Context, auth agent.AuthorizedExecution, raw json.RawMessage) Outcome {
	var in struct {
		Path, Version, ExpectedVersion       string
		Offset, Limit, ByteOffset, ByteLimit *int64
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return readFailure(localReadError(product.CodeInvalidArgument, "invalid read range"))
	}
	r := agent.ReadRequest{Identity: in.Path, Version: in.Version, Mode: "lines"}
	if r.Version == "" {
		r.Version = in.ExpectedVersion
	}
	offset, limit := in.Offset, in.Limit
	if in.ByteOffset != nil || in.ByteLimit != nil {
		if offset != nil || limit != nil {
			return readFailure(localReadError(product.CodeInvalidArgument, "mixed read ranges"))
		}
		r.Mode, offset, limit = "bytes", in.ByteOffset, in.ByteLimit
	}
	if offset != nil {
		r.Offset = *offset
	}
	if limit != nil {
		r.Limit = *limit
	}
	if r.Identity == "" || r.Offset < 0 || r.Limit < 0 {
		return readFailure(localReadError(product.CodeInvalidArgument, "invalid read range"))
	}
	result, err := e.operations.Files.Read(ctx, r)
	if err != nil {
		return readFailure(err)
	}
	if result.Version == "" || result.ContentRef == "" || e.operations.Artifacts == nil {
		return readFailure(localReadError(product.CodeResourceUnavailable, "read snapshot is unavailable"))
	}
	if r.Version != "" && result.Version != r.Version {
		return readFailure(localReadError(product.CodeStateConflict, "read version changed"))
	}
	reader, err := e.operations.Artifacts.Open(ctx, agent.ArtifactRead{Authorization: auth, Ref: agent.ArtifactRef{ID: result.ContentRef, Available: true}})
	if err != nil {
		return readFailure(err)
	}
	if reader == nil {
		return readFailure(localReadError(product.CodeResourceUnavailable, "read snapshot is unavailable"))
	}
	defer reader.Close()
	page, err := projectRead(ctx, reader, r, result.Version)
	if err != nil {
		return readFailure(err)
	}
	return encodeFileResult(page)
}

// Projection streams only the requested range plus bounded lookahead. The
// backend snapshot is never sliced twice, and continuation always carries the
// version that supplied these exact bytes.
func projectRead(ctx context.Context, source io.Reader, req agent.ReadRequest, version string) (readPage, error) {
	page := readPage{Mode: req.Mode, Version: version, Encoding: "utf-8"}
	r := bufio.NewReader(source)
	var position int64
	previous := byte('\n')
	// Skip with bounded storage even when an unselected line is very long.
	for skipped := int64(0); skipped < req.Offset; {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		b, err := r.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return page, err
		}
		position++
		previous = b
		if req.Mode == "bytes" || b == '\n' {
			skipped++
		}
	}
	page.ByteOffset, page.ByteEnd = position, position
	maxBytes, maxLines := int64(50<<10), int64(2000)
	if req.Limit > 0 {
		if req.Mode == "bytes" {
			maxBytes = min(maxBytes, req.Limit)
		} else {
			maxLines = min(maxLines, req.Limit)
		}
	}
	data := make([]byte, 0, maxBytes)
	if req.Mode == "bytes" {
		first, err := r.Peek(1)
		if err != nil && err != io.EOF {
			return page, err
		}
		if position > 0 && len(first) > 0 && !utf8.RuneStart(first[0]) {
			return page, localReadError(product.CodeInvalidArgument, "utf8_boundary")
		}
		for page.Lines < maxLines {
			if err := ctx.Err(); err != nil {
				return page, err
			}
			ch, size, err := r.ReadRune()
			if err == io.EOF {
				break
			}
			if err != nil {
				return page, err
			}
			if ch == utf8.RuneError && size == 1 {
				return page, localReadError(product.CodeInvalidArgument, "invalid_utf8")
			}
			if int64(len(data)+size) > maxBytes {
				if len(data) == 0 {
					return page, localReadError(product.CodeInvalidArgument, "byte_limit_too_small")
				}
				page.Truncated, page.Reason = true, "byte_limit"
				break
			}
			data = utf8.AppendRune(data, ch)
			if ch == '\n' {
				page.Lines++
			}
		}
		// A nonempty final fragment counts as a displayed line, including an
		// unterminated source line at EOF. A terminal newline adds no extra line.
		if len(data) > 0 && data[len(data)-1] != '\n' {
			page.Lines++
		}
		page.PartialLine = len(data) > 0 && (previous != '\n' || data[len(data)-1] != '\n' && page.Truncated)
	} else {
		for page.Lines < maxLines {
			line := make([]byte, 0)
			for {
				if err := ctx.Err(); err != nil {
					return page, err
				}
				ch, size, err := r.ReadRune()
				if err == io.EOF {
					break
				}
				if err != nil {
					return page, err
				}
				if ch == utf8.RuneError && size == 1 {
					return page, localReadError(product.CodeInvalidArgument, "invalid_utf8")
				}
				line = utf8.AppendRune(line, ch)
				if int64(len(data)+len(line)) > maxBytes {
					break
				}
				if ch == '\n' {
					break
				}
			}
			if int64(len(data)+len(line)) > maxBytes {
				page.Truncated, page.Reason = true, "byte_limit"
				if len(data) == 0 {
					page.Reason = "line_too_long"
				}
				break
			}
			if len(line) == 0 {
				break
			}
			data = append(data, line...)
			page.Lines++
		}
	}
	if !page.Truncated && page.Lines == maxLines {
		_, err := r.Peek(1)
		if err == nil {
			page.Truncated, page.Reason = true, "line_limit"
		} else if err != io.EOF {
			return page, err
		}
	}
	if err := ctx.Err(); err != nil {
		return page, err
	}
	page.Content, page.ByteEnd = string(data), position+int64(len(data))
	if page.Truncated {
		next := map[string]any{"path": req.Identity, "version": version}
		if req.Mode == "bytes" || page.Reason == "line_too_long" {
			next["byteOffset"], next["byteLimit"] = page.ByteEnd, maxBytes
		} else {
			next["offset"], next["limit"] = req.Offset+page.Lines, maxLines
		}
		page.NextRead = next
	}
	return page, nil
}
