package storage

import product "github.com/ww1489/seasprak/internal/errors"

// ResourceType selects a finite storage namespace, never a caller-owned path.
type ResourceType string

const (
	ResourceCode     ResourceType = "code"
	ResourceWorkflow ResourceType = "workflow"
)

func resourceNamespace(kind ResourceType) (string, error) {
	switch kind {
	case "", ResourceCode:
		return "sessions", nil
	case ResourceWorkflow:
		return "workflow-runs", nil
	default:
		return "", product.NewError(product.CodeInvalidArgument, "unsupported resource type")
	}
}

func (h Header) ResourceID() string {
	if h.ResourceType == ResourceWorkflow {
		return h.RunID
	}
	return h.SessionID
}

// ValidateHeader rejects cross-type identity before a writer is acquired.
// A missing code type is the unchanged format-v1 Code Agent header.
func ValidateHeader(h Header, kind ResourceType, id string) error {
	if _, err := resourceNamespace(kind); err != nil {
		return err
	}
	if kind == "" {
		kind = ResourceCode
	}
	actual := h.ResourceType
	if actual == "" {
		actual = ResourceCode
	}
	if h.RecordType != "header" || h.FormatVersion != 1 || actual != kind || h.ResourceID() != id || kind == ResourceWorkflow && h.SessionID != "" || kind == ResourceCode && h.RunID != "" {
		return product.NewError(product.CodeIncompatibleVersion, "resource header type, identity or format is incompatible")
	}
	return ValidateResourceID(id)
}

func PrepareResourceDir(root string, kind ResourceType, id string) (string, error) {
	return resourceDir(root, kind, id, true)
}
func OpenResourceDir(root string, kind ResourceType, id string) (string, error) {
	return resourceDir(root, kind, id, false)
}
