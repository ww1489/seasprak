package eino

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestP2SelectionSearchToolsUsesEinoMatcher(t *testing.T) {
	matcher, err := NewToolSearchMatcher(t.Context(), []*schema.ToolInfo{{Name: "read_file", Desc: "Read content"}, {Name: "write_file", Desc: "Write content"}, {Name: "findFiles", Desc: "Find a file"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{{`{"query":"+read content","max_results":1}`, []string{"read_file"}}, {`{"query":"select:write_file,read_file"}`, []string{"write_file", "read_file"}}, {`{"query":"nonexistent"}`, nil}} {
		raw, err := matcher.InvokableRun(t.Context(), tc.query)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Matches []string `json:"matches"`
		}
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(result.Matches, tc.want) {
			t.Fatalf("query=%s matches=%v", tc.query, result.Matches)
		}
	}
}
