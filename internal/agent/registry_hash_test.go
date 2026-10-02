package agent_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
)

func TestOrdinaryDefinitionHashRetainsLegacyEmptyWorkflowSlot(t *testing.T) {
	for _, names := range [][]string{nil, {"echo"}} {
		def := agent.AgentDefinition{Name: "worker", Version: "v1", Kind: "agent", Description: "ordinary", Instruction: "work", Delegable: true, Tools: names}
		fields := []any{def.Name, def.Version, def.Kind, def.Description, def.Instruction, "", def.Delegable, ""}
		if len(names) != 0 {
			fields = append(fields, names)
		}
		raw, _ := json.Marshal(fields)
		sum := sha256.Sum256(raw)
		r, err := agent.NewAgentRegistry("main", []agent.AgentDefinition{def})
		if err != nil {
			t.Fatal(err)
		}
		target, err := r.Target("worker", "generation")
		if err != nil || target.Hash != hex.EncodeToString(sum[:]) {
			t.Fatalf("ordinary definition hash changed: %+v %v", target, err)
		}
		if _, err = r.Resolve(target); err != nil {
			t.Fatal(err)
		}
	}
}
