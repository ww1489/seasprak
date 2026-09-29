package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
)

func TestDiscoveryModelContentEscapingBudget(t *testing.T) {
	for _, escaped := range []string{`"`, `\`} {
		for _, kind := range []string{"list", "glob", "content", "count", "files_with_matches"} {
			t.Run(fmt.Sprintf("%s/%q", kind, escaped), func(t *testing.T) {
				var list agent.ListResult
				var search agent.SearchResult
				for i := 0; i < 100; i++ {
					identity := fmt.Sprintf("%03d%s", i, strings.Repeat(escaped, 500))
					list.Entries = append(list.Entries, agent.FileEntry{Identity: identity, Name: identity})
					search.Matches = append(search.Matches, agent.SearchMatch{Identity: identity, Line: 1, Preview: strings.Repeat(escaped, 500)})
				}
				var out Outcome
				switch kind {
				case "list":
					out = projectList(list, 500)
				case "glob":
					out = projectSearch(search, "glob", "content", 0, 1000)
				default:
					out = projectSearch(search, "grep", kind, 0, 100)
				}
				if !out.Truncated || out.Status != "succeeded" {
					t.Fatalf("status=%s truncated=%v", out.Status, out.Truncated)
				}
				assertDiscoveryModelContent(t, out)
			})
		}
	}
}

// Preserve the existing wire contract: normal results are discovery pages;
// truncated results wrap the same page JSON in content with truncated=true.
func TestDiscoveryModelContentCompatibleShapes(t *testing.T) {
	for _, cursor := range []string{"", "next"} {
		for _, kind := range []string{"list", "search"} {
			t.Run(kind+"/"+cursor, func(t *testing.T) {
				var out Outcome
				if kind == "list" {
					out = projectList(agent.ListResult{Entries: []agent.FileEntry{{Identity: "a", Name: "a"}}, NextCursor: cursor}, 500)
				} else {
					out = projectSearch(agent.SearchResult{Matches: []agent.SearchMatch{{Identity: "a", Preview: "ok"}}, NextCursor: cursor}, "grep", "content", 0, 100)
				}
				if out.Truncated != (cursor != "") {
					t.Fatalf("truncated=%v cursor=%q", out.Truncated, cursor)
				}
				assertDiscoveryModelContent(t, out)
			})
		}
	}
}

func assertDiscoveryModelContent(t *testing.T, out Outcome) {
	t.Helper()
	// The observation is the durable execution fact; exercise its JSON round
	// trip and the same recovery conversion used by the executor.
	observation := observationOf(out)
	data, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	var restored agent.ToolObservation
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored != *observation {
		t.Fatal("observation facts changed after persistence")
	}
	for name, content := range map[string]string{
		"outcome":              out.ModelContent(),
		"observation":          observation.ModelContent(),
		"restored observation": restored.ModelContent(),
		"recovered outcome":    outcomeOfRecord(agent.ToolRecord{Observation: &restored}).ModelContent(),
	} {
		if len(content) > 51200 || !json.Valid([]byte(content)) {
			t.Errorf("%s: invalid bounded JSON: bytes=%d", name, len(content))
			continue
		}
		if content != out.ModelContent() {
			t.Errorf("%s: model content changed after recovery", name)
		}
		pageJSON := content
		if out.Truncated {
			var envelope struct {
				Content   string `json:"content"`
				Truncated bool   `json:"truncated"`
			}
			if err := json.Unmarshal([]byte(content), &envelope); err != nil || !envelope.Truncated {
				t.Fatalf("%s: invalid truncation envelope: %v", name, err)
			}
			pageJSON = envelope.Content
		}
		var page discoveryPage
		if err := json.Unmarshal([]byte(pageJSON), &page); err != nil {
			t.Fatal(err)
		}
		if pageJSON != out.Content || page.Truncated != out.Truncated || page.Returned <= 0 || page.Returned != len(page.Entries)+len(page.Matches)+len(page.Counts)+len(page.Files) {
			t.Fatalf("%s: page shape or facts changed", name)
		}
		if out.Truncated && (page.Hint == "" || len(page.Reasons) == 0) {
			t.Fatalf("%s: missing truncation guidance", name)
		}
	}
}
