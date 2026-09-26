package tools

import (
	"encoding/json"
	"net/url"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	product "github.com/ww1489/seasprak/internal/errors"
)

// Schemas are compiled once for a generation; standalone executors use the
// same compiler. Only documents registered in the generation may be resolved.
type offlineSchemaLoader struct{}

func (offlineSchemaLoader) Load(string) (any, error) {
	return nil, product.NewError(product.CodeInvalidArgument, "unregistered schema reference")
}

func CompileSchemas(defs []Definition) (map[string]*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(offlineSchemaLoader{})
	compiled := make(map[string]*jsonschema.Schema, len(defs))
	for _, def := range defs {
		if def.Name == "" {
			return nil, product.NewError(product.CodeInvalidArgument, "tool name is required")
		}
		if _, ok := compiled[def.Name]; ok {
			return nil, product.NewError(product.CodeInvalidArgument, "duplicate tool name")
		}
		var doc any
		if err := json.Unmarshal(def.Schema, &doc); err != nil {
			return nil, product.NewError(product.CodeInvalidArgument, "tool schema is invalid")
		}
		name := "file:///" + url.PathEscape(def.Name) + ".schema.json"
		if err := compiler.AddResource(name, doc); err != nil {
			return nil, product.NewError(product.CodeInvalidArgument, "tool schema is invalid")
		}
		compiled[def.Name] = nil
	}
	for _, def := range defs {
		schema, err := compiler.Compile("file:///" + url.PathEscape(def.Name) + ".schema.json")
		if err != nil {
			return nil, product.NewError(product.CodeInvalidArgument, "tool schema is invalid or references an unavailable document")
		}
		compiled[def.Name] = schema
	}
	return compiled, nil
}
