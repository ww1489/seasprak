package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strconv"

	store "github.com/ww1489/seasprak/internal/storage"
)

const maxCursorBytes = 512

// EncodeCursor returns the versioned opaque durable position defined by
// 13-p3-web-contract: base64url(JSON([1, sessionId, "durableSeq"])).
func EncodeCursor(sid string, seq uint64) string {
	raw, _ := json.Marshal([]any{1, sid, strconv.FormatUint(seq, 10)})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeCursor accepts only the canonical encoding for sid. It is a position,
// not a capability: callers still authorize the session separately.
func DecodeCursor(sid, cursor string) (uint64, error) {
	if cursor == "" {
		return 0, nil
	}
	if len(cursor) > maxCursorBytes {
		return 0, invalid("cursor is invalid")
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, invalid("cursor is invalid")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var parts []any
	if d.Decode(&parts) != nil || len(parts) != 3 {
		return 0, invalid("cursor is invalid")
	}
	version, ok1 := parts[0].(json.Number)
	owner, ok2 := parts[1].(string)
	text, ok3 := parts[2].(string)
	if !ok1 || !ok2 || !ok3 || version.String() != "1" || store.ValidateResourceID(owner) != nil {
		return 0, invalid("cursor is invalid")
	}
	seq, err := strconv.ParseUint(text, 10, 64)
	if err != nil || EncodeCursor(owner, seq) != cursor {
		return 0, invalid("cursor is invalid")
	}
	if owner != sid {
		return 0, invalid("cursor belongs to another session")
	}
	return seq, nil
}
