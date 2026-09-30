package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	product "github.com/ww1489/seasprak/internal/errors"
)

// Required Markdown headings of a main compaction summary (08 §3).
var SummaryHeadings = []string{"## Goal", "## Constraints & Preferences", "## Progress", "## Key Decisions", "## Next Steps", "## Critical Context"}

// Branch summary headings (08 §3 exploration template): what was tried, what
// was found, what failed, and references. It is never a record of execution
// on the target branch.
var BranchSummaryHeadings = []string{"## Explored", "## Findings", "## Failed Attempts", "## References"}

// CompactionRequest is an automatic request from the execution layer.
// VisibleMessages is the number of non-instruction messages the framework is
// about to send; the session compacts only when it equals the committed
// projection, so no in-flight message can be lost.
type CompactionRequest struct {
	Reason          string // soft_threshold or overflow
	VisibleMessages int
}

// CompactionRequester is the narrow port the model boundary uses to request
// one committed compaction of the active trace's history. It returns the new
// projected path when a compaction was committed.
type CompactionRequester interface {
	CompactForRequest(ctx context.Context, scope ExecutionScope, req CompactionRequest) ([]AgentMessage, bool, error)
}

// CompactionPartition splits the selected path into H (summarized history
// before the current request), P (the current request's completed prefix,
// summarized separately) and K (kept verbatim from FirstKeptID inclusive).
// Previous is the latest summary already on the path; its covered source is
// not re-read.
type CompactionPartition struct {
	Previous    *AgentMessage
	H           []AgentMessage
	P           []AgentMessage
	K           []AgentMessage
	FirstKeptID string
}

// PartitionForCompaction chooses the latest legal cut so K holds at least
// keep non-summary messages. A cut never splits an assistant tool-call message
// from its tool results; a tool result is never a cut point.
func PartitionForCompaction(path []AgentMessage, keep int) (CompactionPartition, error) {
	return PartitionForCompactionInTrace(path, keep, "")
}

// PartitionForCompactionInTrace additionally moves the summarized entries of
// trace traceID (its input U and completed tool groups before the cut) from H
// into P. An empty traceID yields an empty P.
func PartitionForCompactionInTrace(path []AgentMessage, keep int, traceID string) (CompactionPartition, error) {
	part, err := partition(path, keep)
	if err != nil || traceID == "" {
		return part, err
	}
	for i, m := range part.H {
		if m.Scope.TraceID == traceID {
			part.H, part.P = part.H[:i:i], part.H[i:]
			break
		}
	}
	return part, nil
}

func partition(path []AgentMessage, keep int) (CompactionPartition, error) {
	// Work on the effective projection: [previous summary] + uncovered entries.
	view := ProjectHistory(path)
	var previous *AgentMessage
	body := view
	if len(view) > 0 && view[0].Kind == KindCompactionSummary {
		p := view[0]
		previous, body = &p, view[1:]
	}
	cut := len(body) - keep
	if cut <= 0 {
		return CompactionPartition{}, product.NewError(product.CodeStateConflict, "no_op: nothing new to compact")
	}
	for cut > 0 && !legalCut(body, cut) {
		cut--
	}
	if cut == 0 {
		return CompactionPartition{}, product.NewError(product.CodeStateConflict, "no_op: no legal cut point")
	}
	return CompactionPartition{Previous: previous, H: body[:cut], K: body[cut:], FirstKeptID: body[cut].ID}, nil
}

func legalCut(body []AgentMessage, i int) bool {
	if body[i].Kind == KindToolResult {
		return false
	}
	// The message before the cut must not be an assistant awaiting results.
	prev := body[i-1]
	if prev.Kind == KindAssistant && prev.Standard != nil {
		for _, b := range prev.Standard.ContentBlocks {
			if b != nil && b.FunctionToolCall != nil {
				return false
			}
		}
	}
	return true
}

// SerializeForSummary renders messages as bounded, clearly delimited material.
// Tool results stay labelled as tool material even though their role is user.
func SerializeForSummary(msgs []AgentMessage, maxToolBytes int) string {
	var b strings.Builder
	b.WriteString("<history>\n")
	for _, m := range msgs {
		b.WriteString("[" + string(m.Kind) + " " + m.ID + "]\n")
		switch {
		case m.Summary != nil:
			b.WriteString(m.Summary.Text)
		case m.Standard != nil:
			for _, blk := range m.Standard.ContentBlocks {
				switch {
				case blk == nil:
				case blk.UserInputText != nil:
					b.WriteString(blk.UserInputText.Text)
				case blk.AssistantGenText != nil:
					b.WriteString(blk.AssistantGenText.Text)
				case blk.FunctionToolCall != nil:
					b.WriteString("tool_call " + blk.FunctionToolCall.Name + " id=" + blk.FunctionToolCall.CallID + " args=" + blk.FunctionToolCall.Arguments)
				case blk.FunctionToolResult != nil:
					text := ""
					for _, c := range blk.FunctionToolResult.Content {
						if c != nil && c.Text != nil {
							text += c.Text.Text
						}
					}
					if len(text) > maxToolBytes {
						text = text[:maxToolBytes] + "\n[tool output truncated for summary]"
					}
					b.WriteString("tool_result id=" + blk.FunctionToolResult.CallID + "\n" + text)
				}
				b.WriteString("\n")
			}
		case m.Command != nil && !m.Command.ExcludeFromContext:
			b.WriteString("command " + m.Command.Name)
		}
		b.WriteString("\n")
	}
	b.WriteString("</history>\n")
	return b.String()
}

// ValidateSummary requires every heading exactly once and rejects forged
// file appendix markers, which only code may write.
func ValidateSummary(text string) error {
	return validateHeadings(text, SummaryHeadings)
}

// ValidateBranchSummary applies the same rules to the branch template.
func ValidateBranchSummary(text string) error {
	return validateHeadings(text, BranchSummaryHeadings)
}

// StripFileAppendix removes the code-written appendix so a carried summary
// can receive a fresh one without duplicating the reserved marker.
func StripFileAppendix(text string) string {
	if i := strings.Index(text, "\n\n"+fileAppendixMarker); i >= 0 {
		return text[:i]
	}
	return text
}

// ValidateHeadings requires every heading exactly once and rejects forged
// appendix markers; it is shared by the summary templates.
func ValidateHeadings(text string, headings []string) error {
	return validateHeadings(text, headings)
}

func validateHeadings(text string, headings []string) error {
	counts := map[string]int{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		counts[strings.TrimSpace(line)]++
	}
	for _, h := range headings {
		if counts[h] != 1 {
			return product.Errorf(product.CodeInvalidArgument, "summary heading %q is missing or repeated", h)
		}
	}
	if strings.Contains(text, fileAppendixMarker) {
		return product.NewError(product.CodeInvalidArgument, "summary contains a reserved appendix marker")
	}
	return nil
}

// FileFact is one confirmed file operation from a controlled tool.
type FileFact struct {
	Path       string `json:"path"`
	Operation  string `json:"operation"` // read or modified
	ToolCallID string `json:"toolCallId"`
}

// FileDetails is the deterministic appendix source kept in summary details.
type FileDetails struct {
	Read     []string   `json:"read"`
	Modified []string   `json:"modified"`
	Facts    []FileFact `json:"facts"`
}

const fileAppendixMarker = "<!-- seasprak:files -->"

// MergeFileFacts combines the previous trusted details with new confirmed
// facts: modified wins over read; sets are sorted; paths keep their case.
func MergeFileFacts(previous FileDetails, facts []FileFact) FileDetails {
	read, modified := map[string]bool{}, map[string]bool{}
	for _, p := range previous.Read {
		read[p] = true
	}
	for _, p := range previous.Modified {
		modified[p] = true
	}
	out := FileDetails{Facts: append(append([]FileFact(nil), previous.Facts...), facts...)}
	for _, f := range facts {
		switch f.Operation {
		case "read":
			read[f.Path] = true
		case "modified":
			modified[f.Path] = true
		}
	}
	for p := range modified {
		delete(read, p)
		out.Modified = append(out.Modified, p)
	}
	for p := range read {
		out.Read = append(out.Read, p)
	}
	slices.Sort(out.Read)
	slices.Sort(out.Modified)
	return out
}

// FileAppendix renders a bounded appendix; the complete sets stay in details.
func FileAppendix(d FileDetails, max int) string {
	if len(d.Read) == 0 && len(d.Modified) == 0 {
		return ""
	}
	list := func(xs []string) string {
		if len(xs) > max {
			return strings.Join(xs[:max], "\n") + "\n(" + itoa(len(xs)-max) + " more in details)"
		}
		return strings.Join(xs, "\n")
	}
	return "\n\n" + fileAppendixMarker + "\n<read-files>\n" + list(d.Read) + "\n</read-files>\n<modified-files>\n" + list(d.Modified) + "\n</modified-files>"
}

func itoa(n int) string { raw, _ := json.Marshal(n); return string(raw) }

// FileFactsFromCalls extracts confirmed read/write/edit facts from the
// structured arguments of controlled file tools. Denied, unexecuted and
// unknown-effect calls produce no success fact; shell output is never parsed.
func FileFactsFromCalls(calls []ToolRecord) []FileFact {
	var facts []FileFact
	for _, c := range calls {
		obs := c.Observation
		if obs == nil || !obs.Executed || obs.SideEffect == "unknown" || obs.Status == "outcome_unknown" || obs.Status == "denied" {
			continue
		}
		op := ""
		switch c.Call.Name {
		case "read_file":
			op = "read"
		case "write_file", "edit_file":
			op = "modified"
		default:
			continue
		}
		if op == "read" && obs.Status != "succeeded" {
			continue
		}
		var args struct {
			Path     string `json:"path"`
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal([]byte(c.Call.Arguments), &args) != nil {
			continue
		}
		path := args.Path
		if path == "" {
			path = args.FilePath
		}
		if path != "" {
			facts = append(facts, FileFact{Path: path, Operation: op, ToolCallID: c.Call.CallID})
		}
	}
	return facts
}
