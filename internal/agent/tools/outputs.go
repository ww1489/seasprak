package tools

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/agent"
)

// OutputLimits are the P2 limits for model-facing textual projections. Full
// output belongs in an artifact; these values only bound the projection.
type OutputLimits struct {
	MaxLines      int
	MaxBytes      int
	MaxMatches    int
	MaxMatchBytes int
}

func DefaultOutputLimits() OutputLimits {
	return OutputLimits{MaxLines: 2000, MaxBytes: 50 << 10, MaxMatches: 100, MaxMatchBytes: 500}
}

type Preview struct {
	Text       string
	Truncated  bool
	StartLine  int
	EndLine    int
	NextLine   int
	NextOffset int64
	Reason     string
}

// PreviewHeadTail preserves both ends of a log within a single combined
// line/byte budget. Short output is returned verbatim. The truncation marker
// counts against both budgets; tiny budgets use a bounded head instead.
func PreviewHeadTail(text string, maxLines, maxBytes int) Preview {
	if maxLines <= 0 || maxBytes <= 0 {
		return Preview{Truncated: text != "", Reason: previewReason(text != "")}
	}
	if len(text) <= maxBytes && len(splitPreviewLines(text)) <= maxLines {
		return Preview{Text: text, StartLine: 1, EndLine: len(splitPreviewLines(text))}
	}
	const marker = "\n[truncated]\n"
	if maxLines < 3 || maxBytes <= len(marker) {
		out := PreviewHead(text, maxLines, maxBytes)
		out.Truncated, out.Reason = true, "output_limit"
		return out
	}
	bytesLeft := maxBytes - len(marker)
	linesLeft := maxLines - 1
	head := PreviewHead(text, (linesLeft+1)/2, (bytesLeft+1)/2)
	tail := PreviewTail(text, linesLeft/2, bytesLeft/2)
	return Preview{Text: head.Text + marker + tail.Text, Truncated: true, StartLine: 1, EndLine: tail.EndLine, Reason: "output_limit"}
}

// PreviewHead returns complete UTF-8 lines from the beginning until either
// limit is reached. A long individual line is cut only at a UTF-8 boundary.
func PreviewHead(text string, maxLines, maxBytes int) Preview {
	if maxLines <= 0 || maxBytes <= 0 {
		return Preview{Truncated: text != "", StartLine: 1, NextLine: 1, Reason: "limit"}
	}
	lines := splitPreviewLines(text)
	return previewFromLines(lines, maxLines, maxBytes, false)
}

// PreviewTail returns complete UTF-8 lines from the end until either limit is
// reached. It is used for process logs, where the latest output is useful.
func PreviewTail(text string, maxLines, maxBytes int) Preview {
	if maxLines <= 0 || maxBytes <= 0 {
		return Preview{Truncated: text != "", StartLine: 1, NextLine: 1, Reason: "limit"}
	}
	lines := splitPreviewLines(text)
	if len(lines) == 0 {
		return Preview{}
	}
	start := max(0, len(lines)-maxLines)
	selected := append([]string(nil), lines[start:]...)
	truncated := start > 0
	for len(strings.Join(selected, "\n")) > maxBytes && len(selected) > 0 {
		if len(selected) == 1 {
			selected[0] = utf8Suffix(selected[0], maxBytes)
			truncated = true
			break
		}
		selected = selected[1:]
		start++
		truncated = true
	}
	textOut := strings.Join(selected, "\n")
	return Preview{Text: textOut, Truncated: truncated, StartLine: start + 1, EndLine: start + len(selected), NextLine: start + 1, Reason: previewReason(truncated)}
}

func splitPreviewLines(text string) []string {
	if text == "" {
		return nil
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func previewFromLines(lines []string, maxLines, maxBytes int, _ bool) Preview {
	if len(lines) == 0 {
		return Preview{}
	}
	selected := make([]string, 0, min(len(lines), maxLines))
	used := 0
	for i, line := range lines {
		if i >= maxLines {
			break
		}
		additional := len(line)
		if len(selected) > 0 {
			additional++
		}
		if used+additional <= maxBytes {
			selected = append(selected, line)
			used += additional
			continue
		}
		if len(selected) == 0 {
			selected = append(selected, utf8Prefix(line, maxBytes))
		}
		break
	}
	truncated := len(selected) < len(lines) || (len(selected) == 1 && len(selected[0]) < len(lines[0]))
	if len(selected) == maxLines && len(selected) < len(lines) {
		truncated = true
	}
	return Preview{Text: strings.Join(selected, "\n"), Truncated: truncated, StartLine: 1, EndLine: len(selected), NextLine: len(selected) + 1, Reason: previewReason(truncated)}
}

func utf8Prefix(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	text = text[:maxBytes]
	for !utf8.ValidString(text) && len(text) > 0 {
		_, size := utf8.DecodeLastRuneInString(text)
		if size == 1 {
			text = text[:len(text)-1]
			continue
		}
		break
	}
	return text
}

func utf8Suffix(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	text = text[len(text)-maxBytes:]
	for !utf8.ValidString(text) && len(text) > 0 {
		_, size := utf8.DecodeRuneInString(text)
		if size == 1 {
			text = text[1:]
			continue
		}
		break
	}
	return text
}

func previewReason(truncated bool) string {
	if truncated {
		return "output_limit"
	}
	return ""
}

// SaveOutputArtifact records a full backend-produced output reference. It is
// deliberately a thin port call: the executor never reads or recreates output
// from the model-facing preview.
func SaveOutputArtifact(ctx context.Context, store agent.ArtifactStore, auth agent.AuthorizedExecution, contentRef, mediaType, name string) (agent.ArtifactRef, error) {
	if store == nil {
		return agent.ArtifactRef{}, ErrArtifactStoreUnavailable
	}
	return store.Save(ctx, agent.ArtifactInput{Authorization: auth, ContentRef: contentRef, MediaType: mediaType, Name: name})
}

// ErrArtifactStoreUnavailable is returned before a tool is rerun when a full
// output cannot be committed.
var ErrArtifactStoreUnavailable = errArtifactStoreUnavailable{}

type errArtifactStoreUnavailable struct{}

func (errArtifactStoreUnavailable) Error() string { return "artifact store is unavailable" }
