package sessions

import (
	"bytes"
	"encoding/json"
	"sort"

	product "github.com/ww1489/seasprak/internal/errors"
)

func canonicalJSON(raw []byte) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", product.NewError(product.CodeInvalidArgument, "json value is required")
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", product.NewError(product.CodeInvalidArgument, "json value is invalid")
	}
	normalizeJSON(v)
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func normalizeJSON(v any) {
	switch n := v.(type) {
	case map[string]any:
		if req, ok := n["required"].([]any); ok {
			sort.Slice(req, func(i, j int) bool { return jsonString(req[i]) < jsonString(req[j]) })
			n["required"] = req
		}
		for _, child := range n {
			normalizeJSON(child)
		}
	case []any:
		for _, child := range n {
			normalizeJSON(child)
		}
	}
}

func jsonString(v any) string {
	s, _ := v.(string)
	return s
}
