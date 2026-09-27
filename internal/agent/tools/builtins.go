package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ww1489/seasprak/internal/agent"
)

// BuiltinOptions describes the controlled backends made available to the
// built-in tool definitions. The definitions themselves never reach around
// these ports to access the host.
type BuiltinOptions struct {
	Files     agent.FileOperations
	Process   agent.ProcessOperations
	Artifacts agent.ArtifactStore
}

// NewBuiltinDefinitions returns the stable P2 file/process tool definitions.
// Backend availability is checked by Executor immediately before a claim; a
// definition being visible is not permission to execute it.
func NewBuiltinDefinitions(_ BuiltinOptions) []Definition {
	return []Definition{
		builtinDefinition("ls", "List a directory using the controlled file backend.", `{"type":"object","properties":{"root":{"type":"string"},"cursor":{"type":"string"}},"additionalProperties":false}`, "file-operations", "read", func(raw json.RawMessage, d ExecutionDescription) (ExecutionDescription, error) {
			var in struct{ Root, Cursor string }
			if err := json.Unmarshal(raw, &in); err != nil || in.Root == "" {
				return d, fmt.Errorf("root is required")
			}
			d.Resources = []agent.ExecutionResource{{Identity: "path:" + in.Root}}
			d.Effect, d.Concurrency = "read", "shared"
			return d, nil
		}),
		builtinDefinition("read_file", "Read a bounded file range through the controlled file backend.", `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1},"version":{"type":"string"}},"required":["path"],"additionalProperties":false}`, "file-operations", "read", func(raw json.RawMessage, d ExecutionDescription) (ExecutionDescription, error) {
			var in struct {
				Path, Version string
				Offset, Limit int64
			}
			if err := json.Unmarshal(raw, &in); err != nil || in.Path == "" || in.Offset < 0 || in.Limit < 0 {
				return d, fmt.Errorf("path and non-negative range are required")
			}
			d.Resources = []agent.ExecutionResource{{Identity: "path:" + in.Path, ExpectedVersion: in.Version}}
			d.Effect, d.Concurrency = "read", "shared"
			return d, nil
		}),
		builtinDefinition("write_file", "Write content through the controlled file backend.", `{"type":"object","properties":{"path":{"type":"string"},"contentRef":{"type":"string"},"expectedVersion":{"type":"string"}},"required":["path","contentRef"],"additionalProperties":false}`, "file-operations", "write", func(raw json.RawMessage, d ExecutionDescription) (ExecutionDescription, error) {
			var in struct{ Path, ContentRef, ExpectedVersion string }
			if err := json.Unmarshal(raw, &in); err != nil || in.Path == "" || in.ContentRef == "" {
				return d, fmt.Errorf("path and contentRef are required")
			}
			d.Resources = []agent.ExecutionResource{{Identity: "path:" + in.Path, ExpectedVersion: in.ExpectedVersion}}
			d.Effect, d.Concurrency = "write", "exclusive"
			return d, nil
		}),
		builtinDefinition("edit_file", "Apply an exact edit through the controlled file backend.", `{"type":"object","properties":{"path":{"type":"string"},"patchRef":{"type":"string"},"expectedVersion":{"type":"string"}},"required":["path","patchRef"],"additionalProperties":false}`, "file-operations", "write", func(raw json.RawMessage, d ExecutionDescription) (ExecutionDescription, error) {
			var in struct{ Path, PatchRef, ExpectedVersion string }
			if err := json.Unmarshal(raw, &in); err != nil || in.Path == "" || in.PatchRef == "" {
				return d, fmt.Errorf("path and patchRef are required")
			}
			d.Resources = []agent.ExecutionResource{{Identity: "path:" + in.Path, ExpectedVersion: in.ExpectedVersion}}
			d.Effect, d.Concurrency = "write", "exclusive"
			return d, nil
		}),
		builtinDefinition("glob", "Search paths through the controlled file backend.", `{"type":"object","properties":{"root":{"type":"string"},"pattern":{"type":"string"},"cursor":{"type":"string"},"limit":{"type":"integer","minimum":1}},"required":["root","pattern"],"additionalProperties":false}`, "file-operations", "read", func(raw json.RawMessage, d ExecutionDescription) (ExecutionDescription, error) {
			var in struct {
				Root, Pattern, Cursor string
				Limit                 int
			}
			if err := json.Unmarshal(raw, &in); err != nil || in.Root == "" || in.Pattern == "" || in.Limit < 0 {
				return d, fmt.Errorf("root and pattern are required")
			}
			d.Resources = []agent.ExecutionResource{{Identity: "path:" + in.Root}}
			d.Effect, d.Concurrency = "read", "shared"
			return d, nil
		}),
		builtinDefinition("grep", "Search file content through the controlled file backend.", `{"type":"object","properties":{"root":{"type":"string"},"query":{"type":"string"},"cursor":{"type":"string"},"limit":{"type":"integer","minimum":1}},"required":["root","query"],"additionalProperties":false}`, "file-operations", "read", func(raw json.RawMessage, d ExecutionDescription) (ExecutionDescription, error) {
			var in struct {
				Root, Query, Cursor string
				Limit               int
			}
			if err := json.Unmarshal(raw, &in); err != nil || in.Root == "" || in.Query == "" || in.Limit < 0 {
				return d, fmt.Errorf("root and query are required")
			}
			d.Resources = []agent.ExecutionResource{{Identity: "path:" + in.Root}}
			d.Effect, d.Concurrency = "read", "shared"
			return d, nil
		}),
		builtinDefinition("write_todos", "Persist invocation-scoped TODO items through the controlled TODO backend.", `{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"title":{"type":"string"},"state":{"type":"string"}},"required":["title","state"],"additionalProperties":false}}},"required":["items"],"additionalProperties":false}`, "todo-operations", "write", func(raw json.RawMessage, d ExecutionDescription) (ExecutionDescription, error) {
			var in struct{ Items []json.RawMessage }
			if err := json.Unmarshal(raw, &in); err != nil {
				return d, fmt.Errorf("items are required")
			}
			d.Resources = []agent.ExecutionResource{{Identity: "invocation-todos"}}
			d.Effect, d.Concurrency = "write", "exclusive"
			return d, nil
		}),
		builtinDefinition("execute", "Execute an explicit argv command through the controlled process backend.", `{"type":"object","properties":{"argv":{"type":"array","items":{"type":"string"},"minItems":1},"shell":{"type":"string"},"cwd":{"type":"string"},"environmentRef":{"type":"string"},"stdinRef":{"type":"string"},"outputLimitBytes":{"type":"integer","minimum":1}},"required":["argv"],"additionalProperties":false}`, "process-operations", "unknown", func(raw json.RawMessage, d ExecutionDescription) (ExecutionDescription, error) {
			var in struct {
				Argv                                 []string
				Shell, Cwd, EnvironmentRef, StdinRef string
				OutputLimitBytes                     int
			}
			if err := json.Unmarshal(raw, &in); err != nil || len(in.Argv) == 0 || in.OutputLimitBytes < 0 {
				return d, fmt.Errorf("argv is required")
			}
			for _, arg := range in.Argv {
				if strings.IndexByte(arg, 0) >= 0 {
					return d, fmt.Errorf("argv contains NUL")
				}
			}
			d.Argv = append([]string(nil), in.Argv...)
			d.Shell, d.Cwd, d.EnvironmentRef, d.StdinRef = in.Shell, in.Cwd, in.EnvironmentRef, in.StdinRef
			d.OutputLimitBytes = in.OutputLimitBytes
			d.Resources = []agent.ExecutionResource{{Identity: "workspace"}}
			d.Effect, d.Concurrency = "unknown", "exclusive"
			return d, nil
		}),
	}
}

func builtinDefinition(name, description, schema, backend, effect string, resolve func(json.RawMessage, ExecutionDescription) (ExecutionDescription, error)) Definition {
	return Definition{
		Version: name + "-v1", Name: name, Description: description, Schema: json.RawMessage(schema),
		Execution: ExecutionDescription{BackendID: backend, Effect: effect, Concurrency: "shared"},
		ResolveExecution: func(_ context.Context, raw json.RawMessage, d ExecutionDescription) (ExecutionDescription, error) {
			return resolve(raw, d)
		},
	}
}
