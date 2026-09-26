package sessions

import (
	"encoding/json"

	"github.com/cloudwego/eino/schema"
	einojson "github.com/eino-contrib/jsonschema"

	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
)

func alignTools(opts *Options) ([]toolDecl, error) {
	if len(opts.Tools) == 0 {
		if len(opts.ToolInfos) != 0 {
			return nil, product.NewError(product.CodeInvalidArgument, "tool info does not match tool definitions")
		}
		opts.ToolInfos = nil
		return nil, nil
	}
	decls := make([]toolDecl, 0, len(opts.Tools))
	infos := make([]*schema.ToolInfo, 0, len(opts.Tools))
	seen := map[string]struct{}{}
	for _, def := range opts.Tools {
		if def.Run != nil && def.RunWithOutput != nil {
			return nil, product.NewError(product.CodeInvalidArgument, "tool has conflicting execution callbacks")
		}
		if def.Name == "" || def.Version == "" {
			return nil, product.NewError(product.CodeInvalidArgument, "tool name and implementation version are required")
		}
		if _, ok := seen[def.Name]; ok {
			return nil, product.Errorf(product.CodeInvalidArgument, "duplicate tool %s", def.Name)
		}
		seen[def.Name] = struct{}{}
		if !tools.ValidToolInterface(def.ToolInterface) {
			return nil, product.NewError(product.CodeInvalidArgument, "tool interface is unsupported")
		}
		if def.ToolInterface == "streamable" || def.ToolInterface == "enhanced-streamable" {
			return nil, product.NewError(product.CodeResourceUnavailable, "streaming tool lifecycle is unavailable")
		}
		info, err := toolInfoFromDefinition(def)
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
		schemaText, err := canonicalJSON(def.Schema)
		if err != nil {
			return nil, product.NewError(product.CodeInvalidArgument, "tool schema is invalid")
		}
		static := def.Execution.Clone()
		decls = append(decls, toolDecl{
			Name: def.Name, Version: def.Version, Description: def.Description, Schema: json.RawMessage(schemaText), ToolInterface: def.ToolInterface, Execution: &static, OutputCallback: def.RunWithOutput != nil,
		})
	}
	if err := matchToolInfos(decls, opts.ToolInfos); err != nil {
		return nil, err
	}
	compiled, err := tools.CompileSchemas(opts.Tools)
	if err != nil {
		return nil, err
	}
	opts.compiledTools = compiled
	opts.ToolInfos = infos
	return decls, nil
}

func toolInfoFromDefinition(def tools.Definition) (*schema.ToolInfo, error) {
	var js einojson.Schema
	if err := json.Unmarshal(def.Schema, &js); err != nil {
		return nil, product.NewError(product.CodeInvalidArgument, "tool schema is invalid")
	}
	return &schema.ToolInfo{Name: def.Name, Desc: def.Description, ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&js)}, nil
}

func matchToolInfos(decls []toolDecl, infos []*schema.ToolInfo) error {
	if len(infos) == 0 {
		return nil
	}
	if len(infos) != len(decls) {
		return product.NewError(product.CodeInvalidArgument, "tool info does not match tool definitions")
	}
	byName := map[string]struct{ schema, desc string }{}
	for _, info := range infos {
		if info == nil {
			return product.NewError(product.CodeInvalidArgument, "tool info does not match tool definitions")
		}
		text, err := infoSchema(info)
		if err != nil {
			return err
		}
		if _, ok := byName[info.Name]; ok {
			return product.NewError(product.CodeInvalidArgument, "tool info does not match tool definitions")
		}
		byName[info.Name] = struct{ schema, desc string }{text, info.Desc}
	}
	for _, decl := range decls {
		got, ok := byName[decl.Name]
		if !ok || got.schema != string(decl.Schema) || got.desc != decl.Description {
			return product.NewError(product.CodeInvalidArgument, "tool info does not match tool definitions")
		}
	}
	return nil
}

func infoSchema(info *schema.ToolInfo) (string, error) {
	if info.ParamsOneOf == nil {
		return "", product.NewError(product.CodeInvalidArgument, "tool info schema is missing")
	}
	js, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(js)
	if err != nil {
		return "", err
	}
	return canonicalJSON(raw)
}

func matchGeneration(opts *Options, generation string) error {
	loaded, err := loadManifest(opts.StateRoot, opts.SessionID, generation)
	if err != nil {
		return err
	}
	decls, err := alignTools(opts)
	if err != nil {
		return err
	}
	legacy := len(loaded.Tools) > 0
	for _, tool := range loaded.Tools {
		if tool.Execution != nil {
			legacy = false
			break
		}
	}
	if legacy {
		// Earlier manifests did not record execution declarations. Preserve their
		// original hash and explicit Version contract without rewriting history.
		for i := range decls {
			decls[i].Execution = nil
		}
	}
	current, err := buildManifest(loaded.ID, decls, opts.GenerationFingerprint)
	if err != nil {
		return err
	}
	if loaded.Runtime != current.Runtime {
		return product.NewError(product.CodeIncompatibleResume, "runtime build fingerprint does not match the generation")
	}
	if loaded.Hash != current.Hash || loaded.Model != opts.GenerationFingerprint {
		return product.NewError(product.CodeIncompatibleVersion, "tool generation does not match the saved manifest")
	}
	return nil
}
