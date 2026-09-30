package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/ww1489/seasprak/internal/config"
)

const MaxJSONBytes = config.WebJSONBytes

// DecodeJSON accepts one bounded JSON object. Route DTOs must use explicit
// lowerCamelCase tags and must not contain product-internal fields.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxJSONBytes)
	return decodeObject(r.Body, dst)
}

func decodeObject(r io.Reader, dst any) error {
	// Read the whole bounded input so trailing whitespace cannot bypass the limit.
	b, err := io.ReadAll(io.LimitReader(r, MaxJSONBytes+1))
	if err != nil || len(b) > MaxJSONBytes {
		return invalid("JSON body exceeds limit or cannot be read")
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return invalid("one JSON object is required")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err = d.Decode(dst); err != nil {
		return invalid("invalid JSON object")
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return invalid("one JSON object is required")
	}
	return nil
}
