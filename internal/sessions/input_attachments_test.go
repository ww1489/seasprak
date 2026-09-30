package sessions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

// modalModel captures model input and declares its modalities through the
// same Configuration contract as catalog models.
type modalModel struct {
	*testkit.FakeModel
	mu     sync.Mutex
	image  bool
	inputs [][]*schema.AgenticMessage
}

func (m *modalModel) Configuration() llm.ModelConfig {
	items := map[llm.CapabilityName]llm.Capability{llm.CapText: {Status: llm.Declared}}
	if m.image {
		items[llm.CapInputImage] = llm.Capability{Status: llm.Declared}
	}
	return llm.ModelConfig{Model: "modal", Version: "modal-v1", Capabilities: llm.ModelCapabilities{Items: items}}
}
func (m *modalModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	m.mu.Lock()
	m.inputs = append(m.inputs, in)
	m.mu.Unlock()
	return m.FakeModel.Generate(ctx, in, opts...)
}
func (m *modalModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")

func attachmentSession(t *testing.T, m *modalModel) (*Catalog, *AgentSession, string) {
	t.Helper()
	opts, _ := catalogOptions(t)
	opts.Model = m
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	res, err := c.Create(t.Context(), CatalogCreateRequest{IdempotencyKey: "s", Workspace: opts.Workspace, ModelRef: DefaultModelRef})
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Writer(t.Context(), res.Snapshot.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return c, s, res.Snapshot.SessionID
}

func attachmentInput(text string, ids ...string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"text": text, "attachments": ids})
	return raw
}

func TestInputAttachmentsReachModelAsContentBlocks(t *testing.T) {
	m := &modalModel{FakeModel: testkit.NewFake(), image: true}
	c, s, sid := attachmentSession(t, m)
	note, _, err := c.SaveAttachment(t.Context(), sid, SaveAttachmentRequest{IdempotencyKey: "n", MimeType: "text/plain", Name: "notes.txt", Content: []byte("secret-free note body")})
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := c.SaveAttachment(t.Context(), sid, SaveAttachmentRequest{IdempotencyKey: "i", MimeType: "image/png", Content: pngBytes})
	if err != nil {
		t.Fatal(err)
	}
	in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: attachmentInput("look", note.ArtifactID, img.ArtifactID)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	if m.Calls() != 1 || len(m.inputs) != 1 {
		t.Fatalf("model calls=%d", m.Calls())
	}
	var sawText, sawImage bool
	for _, msg := range m.inputs[0] {
		for _, b := range msg.ContentBlocks {
			if b.UserInputText != nil && strings.Contains(b.UserInputText.Text, "secret-free note body") && strings.Contains(b.UserInputText.Text, `name="notes.txt"`) {
				sawText = true
			}
			if b.UserInputImage != nil && b.UserInputImage.MIMEType == "image/png" && b.UserInputImage.Base64Data != "" {
				sawImage = true
			}
		}
	}
	if !sawText || !sawImage {
		t.Fatalf("model input lacks attachment blocks: text=%t image=%t", sawText, sawImage)
	}
	// History keeps references only; attachment bytes never enter the journal.
	journal, err := os.ReadFile(filepath.Join(c.opts.StateRoot, "sessions", sid, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(journal), "secret-free note body") || !strings.Contains(string(journal), note.ArtifactID) {
		t.Fatal("journal copied attachment content or lost its reference")
	}
}

func TestInputAttachmentsRejectedBeforeAcceptance(t *testing.T) {
	m := &modalModel{FakeModel: testkit.NewFake(), image: false}
	c, s, sid := attachmentSession(t, m)
	img, _, err := c.SaveAttachment(t.Context(), sid, SaveAttachmentRequest{IdempotencyKey: "i", MimeType: "image/png", Content: pngBytes})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, code string
		cmd        agent.InputCommand
	}{
		{"image without capability", product.CodeUnsupportedCapability, agent.InputCommand{Kind: "prompt", Content: attachmentInput("x", img.ArtifactID)}},
		{"missing id", product.CodeNotFound, agent.InputCommand{Kind: "prompt", Content: attachmentInput("x", strings.Repeat("a", 32))}},
		{"path-shaped id", product.CodeInvalidArgument, agent.InputCommand{Kind: "prompt", Content: attachmentInput("x", "../../journal.jsonl")}},
		{"duplicate id", product.CodeInvalidArgument, agent.InputCommand{Kind: "prompt", Content: attachmentInput("x", img.ArtifactID, img.ArtifactID)}},
	}
	for _, tc := range cases {
		before := s.rt.manager.View().LastSeq
		_, err := s.SubmitInput(t.Context(), tc.cmd)
		if pe, ok := product.AsError(err); !ok || pe.Code != tc.code {
			t.Fatalf("%s: err=%v", tc.name, err)
		}
		if s.rt.manager.View().LastSeq != before {
			t.Fatalf("%s wrote state", tc.name)
		}
	}
	// An attachment of another session is not resolvable here.
	other, err := c.Create(t.Context(), CatalogCreateRequest{IdempotencyKey: "s2", Workspace: c.opts.Workspace, ModelRef: DefaultModelRef})
	if err != nil {
		t.Fatal(err)
	}
	foreign, _, err := c.SaveAttachment(t.Context(), other.Snapshot.SessionID, SaveAttachmentRequest{IdempotencyKey: "t", MimeType: "text/plain", Content: []byte("other")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: attachmentInput("x", foreign.ArtifactID)}); err == nil {
		t.Fatal("cross-session attachment accepted")
	}
	if m.Calls() != 0 {
		t.Fatalf("rejected inputs reached the model %d times", m.Calls())
	}
}
