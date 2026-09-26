package sessions

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestReopenRejectsChangedExecutionFields(t *testing.T) {
	base := tools.ExecutionDescription{BackendID: "process-operations", Effect: "read", Concurrency: "shared", Resources: []agent.ExecutionResource{{Identity: "alpha", ExpectedVersion: "v1"}}, Argv: []string{"test"}, Cwd: "root", EnvironmentRef: "env", StdinRef: "stdin", Mounts: []agent.ExecutionMount{{SourceRef: "source", Target: "target"}}, TempRootRef: "temp", PolicyRef: "policy", RequestedGrantRef: "grant", Timeout: time.Second, OutputLimitBytes: 1024}
	cases := map[string]func(*tools.ExecutionDescription){
		"backend":   func(d *tools.ExecutionDescription) { d.BackendID = "file-operations" },
		"effect":    func(d *tools.ExecutionDescription) { d.Effect = "write" },
		"resources": func(d *tools.ExecutionDescription) { d.Resources[0].Identity = "beta" },
		"timeout":   func(d *tools.ExecutionDescription) { d.Timeout = 2 * time.Second },
		"output":    func(d *tools.ExecutionDescription) { d.OutputLimitBytes = 2048 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: base.Clone()}
			opts := Options{SessionID: "changed-" + name, Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: testkit.NewFake(), Tools: []tools.Definition{def}}
			session, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			changed := base.Clone()
			change(&changed)
			opts.Tools[0].Execution = changed
			reopened, err := OpenAgentSession(t.Context(), opts)
			if reopened != nil {
				_ = reopened.Close(context.Background())
			}
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
				t.Fatalf("changed %s accepted: %v", name, err)
			}
		})
	}
}

func TestManifestCopiesExecutionDeclaration(t *testing.T) {
	def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Resources: []agent.ExecutionResource{{Identity: "original"}}, Argv: []string{"original"}, Mounts: []agent.ExecutionMount{{SourceRef: "original"}}}}
	opts := Options{Tools: []tools.Definition{def}}
	decl, err := alignTools(&opts)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := buildManifest("", decl, "")
	if err != nil {
		t.Fatal(err)
	}
	original := manifest.Hash
	def.Execution.Resources[0].Identity = "changed"
	def.Execution.Argv[0] = "changed"
	def.Execution.Mounts[0].SourceRef = "changed"
	if manifest.Tools[0].Execution.Resources[0].Identity != "original" || manifest.Tools[0].Execution.Argv[0] != "original" || manifest.Tools[0].Execution.Mounts[0].SourceRef != "original" {
		t.Fatal("manifest declaration aliases definition")
	}
	again, err := buildManifest("", manifest.Tools, "")
	if err != nil || again.Hash != original {
		t.Fatalf("manifest hash changed after caller mutation: %v", err)
	}
}
