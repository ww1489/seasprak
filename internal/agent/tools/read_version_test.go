package tools

import (
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
)

func TestBuiltinReadFilePassesRequestedVersion(t *testing.T) {
	files := &builtinFileProbe{read: agent.ReadResult{ContentRef: "artifact:read", Version: "v2"}}
	artifacts := &builtinArtifactProbe{content: "versioned"}
	def := builtinByName(t, NewBuiltinDefinitions(BuiltinOptions{}), "read_file")
	const args = `{"path":"file.txt","version":"v2"}`
	sink := &recordSink{found: true, rec: builtinAccepted(args, "read_file", "read-version")}
	executor, err := NewExecutor("gen", []Definition{def}, sink, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Files: files, Artifacts: artifacts}), WithResourceScheduler(NewResourceScheduler()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := executor.Run(t.Context(), agent.ExecutionScope{SessionID: "session"}, "read-version", "read_file", args)
	if err != nil || out.Status != "succeeded" || files.reads.Load() != 1 || files.lastRead.Version != "v2" {
		t.Fatalf("requested version not passed: status=%s reads=%d version=%q err=%v", out.Status, files.reads.Load(), files.lastRead.Version, err)
	}
}
