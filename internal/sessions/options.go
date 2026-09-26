package sessions

import (
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/sessions/store"
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
	GenerationFingerprint string
}
