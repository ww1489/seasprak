package agent

import (
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestMessageUnionAndProjection(t *testing.T) {
	msg := AgentMessage{ID: "m1", Kind: KindUser, Status: StatusComplete, Standard: schema.UserAgenticMessage("hi")}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var back AgentMessage
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := AgentMessage{ID: "m2", Kind: KindUser, Standard: schema.UserAgenticMessage("a"), Custom: &CustomMessage{CustomType: "x"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected invalid union")
	}
	custom := AgentMessage{ID: "c", Kind: KindCustom, Status: StatusComplete, Custom: &CustomMessage{CustomType: "note", Display: false, Content: schema.UserAgenticMessage("secret-visible-to-model")}}
	projected, err := ConvertToLLM([]AgentMessage{custom})
	if err != nil {
		t.Fatal(err)
	}
	if len(projected) != 1 || projected[0].Role != schema.AgenticRoleTypeUser {
		t.Fatalf("display must not control model visibility: %#v", projected)
	}
	pending := AgentMessage{ID: "p", Kind: KindOpaque, Status: StatusComplete, Opaque: &OpaqueMessage{Raw: []byte(`{}`), RequiredForModel: true}}
	if _, err := ConvertToLLM([]AgentMessage{pending}); err == nil {
		t.Fatal("required opaque message must fail")
	}
}
