package store

import (
	"context"
	"encoding/json"
)

// CreationRecord binds a durable creation key to one preallocated session.
// Receipt is the original response snapshot, not a later session view.
type CreationRecord struct {
	SessionID  string          `json:"sessionId"`
	Digest     string          `json:"digest"`
	Generation string          `json:"generation"`
	Receipt    json.RawMessage `json:"receipt,omitempty"`
}

// CreationRegistry has one writer for its lifetime. Records outlive query indexes.
type CreationRegistry interface {
	Load(context.Context, string) (CreationRecord, bool, error)
	Save(context.Context, string, CreationRecord) error
	Close() error
}
