package tools

import (
	"encoding/json"
	"github.com/ww1489/seasprak/internal/agent"
	"sort"
)

const discoveryBytes = 50 * 1024

type discoveryPage struct {
	Entries   []agent.FileEntry   `json:"entries,omitempty"`
	Matches   []agent.SearchMatch `json:"matches,omitempty"`
	Files     []string            `json:"files,omitempty"`
	Counts    []fileMatchCount    `json:"counts,omitempty"`
	Returned  int                 `json:"returned"`
	Truncated bool                `json:"truncated"`
	Reasons   []string            `json:"truncationReasons,omitempty"`
	Hint      string              `json:"hint,omitempty"`
}
type fileMatchCount struct {
	Identity string `json:"identity"`
	Count    int    `json:"count"`
}

func (p *discoveryPage) truncate(reason string) {
	p.Truncated = true
	for _, r := range p.Reasons {
		if r == reason {
			return
		}
	}
	p.Reasons = append(p.Reasons, reason)
	p.Hint = "Results are incomplete; narrow the root or pattern."
}
func (p discoveryPage) size() int { b, _ := json.Marshal(p); return len(b) }
func discoveryOutcome(p discoveryPage) Outcome {
	for {
		out := encodeFileResult(p)
		out.Truncated = p.Truncated
		// The model envelope escapes Content again when truncated. Bound that
		// final representation, retaining whole records and durable truncation
		// facts so ToolObservation.ModelContent has the same bound on replay.
		if len(out.ModelContent()) <= discoveryBytes {
			return out
		}
		p.truncate("byte_limit")
		switch {
		case len(p.Entries) > 0:
			p.Entries = p.Entries[:len(p.Entries)-1]
		case len(p.Matches) > 0:
			p.Matches = p.Matches[:len(p.Matches)-1]
		case len(p.Files) > 0:
			p.Files = p.Files[:len(p.Files)-1]
		case len(p.Counts) > 0:
			p.Counts = p.Counts[:len(p.Counts)-1]
		}
		p.Returned--
	}
}
func projectList(result agent.ListResult, limit int) Outcome {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	entries := append([]agent.FileEntry(nil), result.Entries...)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Identity == entries[j].Identity {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].Identity < entries[j].Identity
	})
	p := discoveryPage{}
	if result.NextCursor != "" {
		p.truncate("backend_limit")
	}
	for _, entry := range entries {
		if p.Returned >= limit {
			p.truncate("entry_limit")
			break
		}
		p.Entries = append(p.Entries, entry)
		p.Returned++
		if p.size() > discoveryBytes-256 {
			p.Entries = p.Entries[:len(p.Entries)-1]
			p.Returned--
			p.truncate("byte_limit")
			break
		}
	}
	return discoveryOutcome(p)
}
func projectSearch(result agent.SearchResult, kind, mode string, offset, limit int) Outcome {
	maxCount := 100
	if kind == "glob" {
		maxCount = 1000
	}
	if limit <= 0 || limit > maxCount {
		limit = maxCount
	}
	matches := append([]agent.SearchMatch(nil), result.Matches...)
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Identity == matches[j].Identity {
			return matches[i].Line < matches[j].Line
		}
		return matches[i].Identity < matches[j].Identity
	})
	p := discoveryPage{}
	if result.NextCursor != "" {
		p.truncate("backend_limit")
	}
	// Aggregate before offset so count mode reports complete per-file counts.
	counts := []fileMatchCount{}
	if mode == "count" || mode == "files_with_matches" {
		for _, m := range matches {
			if len(counts) == 0 || counts[len(counts)-1].Identity != m.Identity {
				counts = append(counts, fileMatchCount{Identity: m.Identity})
			}
			counts[len(counts)-1].Count++
		}
	}
	n := len(matches)
	if mode == "count" || mode == "files_with_matches" {
		n = len(counts)
	}
	if offset > n {
		offset = n
	}
	if offset < 0 {
		offset = 0
	}
	for i := offset; i < n; i++ {
		if p.Returned >= limit {
			p.truncate("entry_limit")
			break
		}
		switch mode {
		case "count":
			p.Counts = append(p.Counts, counts[i])
		case "files_with_matches":
			p.Files = append(p.Files, counts[i].Identity)
		default:
			m := matches[i]
			if kind == "grep" {
				r := []rune(m.Preview)
				if len(r) > 500 {
					m.Preview = string(r[:500])
					p.truncate("line_limit")
				}
			}
			p.Matches = append(p.Matches, m)
		}
		p.Returned++
		if p.size() > discoveryBytes-256 {
			switch mode {
			case "count":
				p.Counts = p.Counts[:len(p.Counts)-1]
			case "files_with_matches":
				p.Files = p.Files[:len(p.Files)-1]
			default:
				p.Matches = p.Matches[:len(p.Matches)-1]
			}
			p.Returned--
			p.truncate("byte_limit")
			break
		}
	}
	return discoveryOutcome(p)
}
