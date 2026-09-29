package operations

import (
	"bytes"
	"context"
	"io"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

type outputArtifact struct {
	binding agent.OutputArtifactBinding
	ref     agent.ArtifactRef
	data    []byte
}

// SaveOutput stores already-redacted text under a trusted executor binding.
// This fixture has no execution authority, host paths, or credential access.
func (m *Memory) SaveOutput(ctx context.Context, in agent.OutputArtifactInput) (agent.ArtifactRef, error) {
	m.count("save_output")
	if err := ctx.Err(); err != nil {
		return agent.ArtifactRef{}, err
	}
	if in.Binding.SessionID == "" || in.Binding.Environment == "" || in.Binding.CallID == "" {
		return agent.ArtifactRef{}, failure(product.CodeInvalidArgument)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return agent.ArtifactRef{}, err
	}
	data := []byte(in.Content)
	ref := agent.ArtifactRef{ID: m.ref("output"), SessionID: in.Binding.SessionID, Environment: in.Binding.Environment, Hash: hash(data), Size: int64(len(data)), Available: true}
	if m.outputArtifacts == nil {
		m.outputArtifacts = map[string]outputArtifact{}
	}
	m.outputArtifacts[ref.ID] = outputArtifact{binding: in.Binding, ref: ref, data: data}
	return ref, nil
}

func (m *Memory) OpenOutput(ctx context.Context, in agent.OutputArtifactRead) (io.ReadCloser, error) {
	m.count("open_output")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	saved, ok := m.outputArtifacts[in.Ref.ID]
	if !ok {
		return nil, failure(product.CodeResourceUnavailable)
	}
	if in.Binding != saved.binding {
		return nil, failure(product.CodePermissionDenied)
	}
	if in.Ref != saved.ref || int64(len(saved.data)) != saved.ref.Size || hash(saved.data) != saved.ref.Hash {
		return nil, failure(product.CodeStateConflict)
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(saved.data))), nil
}

var _ agent.OutputArtifactStore = (*Memory)(nil)
