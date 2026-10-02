package storage

import (
	"context"
	"encoding/json"
)

// CreationRecord binds a durable creation key to exactly one typed resource.
// Receipt is the original response snapshot, not a later resource view.
type CreationRecord struct {
	ResourceType ResourceType    `json:"resourceType,omitempty"`
	SessionID    string          `json:"sessionId,omitempty"`
	RunID        string          `json:"runId,omitempty"`
	Digest       string          `json:"digest"`
	Generation   string          `json:"generation"`
	Receipt      json.RawMessage `json:"receipt,omitempty"`
}

// CreationRegistry has one writer for its lifetime. Records outlive query indexes.
type CreationRegistry interface {
	Load(context.Context, string) (CreationRecord, bool, error)
	Save(context.Context, string, CreationRecord) error
	Close() error
}
