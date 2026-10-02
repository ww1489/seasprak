package codeagent

import (
	"os"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	store "github.com/ww1489/seasprak/internal/storage"
)

const (
	ProfileDefault = ""
	ProfileMemory  = "memory"
)

type Options struct {
	Policy                *agent.ResolvedPolicy
	Operations            tools.Operations
	ResourceScheduler     *tools.ResourceScheduler
	ResourceEnvironment   string
	Workspace             string
	StateRoot             string
	SessionID             string
	Model                 einomodel.AgenticModel
	Tools                 []tools.Definition
	ToolInfos             []*schema.ToolInfo
	compiledTools         map[string]*jsonschema.Schema
	Limits                config.Limits
	Profile               string
	Instruction           string
	Store                 store.Store
	Principal             string
	ReadOnly              bool
	GenerationFingerprint string // Trusted application/SDK build and model/tool/hook/backend implementation version; required for Resume.
	ReconcileQueries      map[string]ReconcileQuery
	// Agents are additional execution targets registered at startup. The
	// built-in main agent always exists; there is no runtime reload.
	Agents []agent.AgentDefinition

	// fileRoot is borrowed only from a successfully opened default file backend
	// and remains valid until that backend's Close. Injected stores do not set it.
	fileRoot *os.Root
}
