package consumer_test

import (
	"encoding/json"
	"github.com/ww1489/seasprak/sdk"
	"testing"
)

func TestSDKConsumerFileDiscoveryAndExactEditContract(t *testing.T) {
	list := sdk.ListRequest{Root: "workspace", Limit: 500}
	search := sdk.SearchRequest{Root: "workspace", Kind: "grep", Query: "needle", Glob: "*.go", OutputMode: "count", Offset: 1, Limit: 100, CaseInsensitive: true}
	edit := sdk.AuthorizedFileEdit{Path: "file", OldString: "before", NewString: "after", ReplaceAll: true, ExpectedVersion: "v1"}
	if list.Limit != 500 || search.Kind != "grep" {
		t.Fatal("public discovery contract unavailable")
	}
	raw, err := json.Marshal(edit)
	if err != nil {
		t.Fatal(err)
	}
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil || args["old_string"] != "before" || args["new_string"] != "after" || args["replace_all"] != true {
		t.Fatal("literal edit fields unavailable")
	}
	for _, d := range sdk.NewBuiltinDefinitions(sdk.BuiltinOptions{}) {
		var s struct {
			Properties map[string]json.RawMessage
			Required   []string
		}
		if err := json.Unmarshal(d.Schema, &s); err != nil {
			t.Fatal(err)
		}
		switch d.Name {
		case "edit_file":
			for _, name := range []string{"old_string", "new_string", "replace_all"} {
				if s.Properties[name] == nil {
					t.Fatalf("missing %s", name)
				}
			}
			if s.Properties["patchRef"] != nil {
				t.Fatal("private patch format exposed in model schema")
			}
		case "ls", "glob":
			if s.Properties["cursor"] != nil || s.Properties["limit"] == nil {
				t.Fatal("unexpected pagination contract")
			}
		case "grep":
			for _, name := range []string{"output_mode", "head_limit", "offset"} {
				if s.Properties[name] == nil {
					t.Fatalf("missing %s", name)
				}
			}
		}
	}
}
