package operations_test

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

type readBindingProbe struct {
	*fixture.Memory
	mutate func(*agent.ReadRequest)
	open   func(context.Context, agent.ArtifactRead) (io.ReadCloser, error)
}

func (p *readBindingProbe) Read(ctx context.Context, r agent.ReadRequest) (agent.ReadResult, error) {
	if p.mutate != nil {
		p.mutate(&r)
	}
	return p.Memory.Read(ctx, r)
}
func (p *readBindingProbe) Open(ctx context.Context, r agent.ArtifactRead) (io.ReadCloser, error) {
	if p.open != nil {
		return p.open(ctx, r)
	}
	return p.Memory.Open(ctx, r)
}

func TestReadSnapshotBindsFrozenRequestAndConsumesOnce(t *testing.T) {
	for _, mode := range []string{"range", "limit", "version", "mode", "legacy-mode", "path", "replay", "artifact-range", "resource"} {
		t.Run(mode, func(t *testing.T) {
			m := fixture.NewMemory()
			version := m.SeedFile("file", []byte("one\ntwo\n"))
			m.SeedFile("other", []byte("one\ntwo\n"))
			p := &readBindingProbe{Memory: m}
			p.mutate = func(r *agent.ReadRequest) {
				switch mode {
				case "range":
					r.Offset++
				case "limit":
					r.Limit++
				case "version":
					r.Version = ""
				case "path":
					r.Identity = "other"
				case "mode", "legacy-mode":
					v := "bytes"
					if mode == "legacy-mode" {
						v = ""
					}
					r.Mode = v
				}
			}
			p.open = func(ctx context.Context, r agent.ArtifactRead) (io.ReadCloser, error) {
				if mode == "artifact-range" {
					r.Offset = 1
				}
				if mode == "resource" {
					r.Authorization.Frozen.Resources[0].Identity = "path:other"
				}
				reader, err := m.Open(ctx, r)
				if mode == "replay" {
					if err != nil {
						t.Fatal(err)
					}
					again, replayErr := m.Open(ctx, r)
					if again != nil {
						again.Close()
					}
					code(t, replayErr, product.CodePermissionDenied)
				} else {
					code(t, err, product.CodePermissionDenied)
				}
				return reader, err
			}
			out := run(t, m, "read_file", map[string]any{"path": "file", "version": version, "limit": 1}, p, p)
			if mode == "replay" {
				if out.Status != "succeeded" || m.Calls("open") != 2 {
					t.Fatalf("valid one-use read failed: %+v", out)
				}
			} else if out.Status != "failed" || out.Executed || m.Calls("open") != 1 {
				t.Fatalf("changed binding executed: %+v", out)
			}
		})
	}
}

func TestReadSnapshotRemainsImmutableDuringOpen(t *testing.T) {
	m := fixture.NewMemory()
	version := m.SeedFile("file", []byte("one\r\ntwo\r\n"))
	p := &readBindingProbe{Memory: m, open: func(ctx context.Context, r agent.ArtifactRead) (io.ReadCloser, error) {
		m.SeedFile("file", []byte("changed"))
		return m.Open(ctx, r)
	}}
	out := run(t, m, "read_file", map[string]any{"path": "file", "version": version}, p, p)
	var page struct{ Content, Version string }
	if out.Status != "succeeded" || json.Unmarshal([]byte(out.Content), &page) != nil || page.Content != "one\r\ntwo\r\n" || page.Version != version || m.Calls("open") != 1 {
		t.Fatal("read did not preserve immutable snapshot")
	}
}
