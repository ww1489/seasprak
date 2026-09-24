package sessions

import (
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

const (
	ProfileDefault = ""
	ProfileMemory  = "memory"
)

type Options struct {
	Workspace             string
	StateRoot             string
	SessionID             string
	Model                 einomodel.AgenticModel
	Tools                 []tools.Definition
	ToolInfos             []*schema.ToolInfo
	Limits                config.Limits
	Profile               string
	Instruction           string
	Store                 store.Store
	Principal             string
	ReadOnly              bool
	GenerationFingerprint string
}
