package codeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/storage"
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

type attachmentFixture struct{ opts Options }
type attachmentFixtureRequest struct {
	IdempotencyKey, MimeType, Name string
	Content                        []byte
}

// Fixtures use neutral records directly, never a Web application catalog.
func (f *attachmentFixture) SaveAttachment(ctx context.Context, sid string, req attachmentFixtureRequest) (storage.Attachment, bool, error) {
	if err := ctx.Err(); err != nil {
		return storage.Attachment{}, false, err
	}
	if err := storage.CheckAttachmentContent(req.MimeType, req.Content); err != nil {
		return storage.Attachment{}, false, err
	}
	key, _ := json.Marshal([]string{f.opts.Principal, sid, req.IdempotencyKey})
	id, sum := sha256.Sum256(key), sha256.Sum256(req.Content)
	rec := storage.Attachment{ArtifactID: hex.EncodeToString(id[:16]), MimeType: req.MimeType, Name: req.Name, Size: int64(len(req.Content)), SHA256: hex.EncodeToString(sum[:]), Digest: "fixture"}
	root, _, err := storage.SessionSubdir(f.opts.StateRoot, sid, "attachments", true)
	if err != nil {
		return storage.Attachment{}, false, err
	}
	defer root.Close()
	raw, _ := json.Marshal(rec)
	for _, file := range []struct {
		name string
		data []byte
	}{{rec.ArtifactID + ".bin", req.Content}, {rec.ArtifactID + ".json", raw}} {
		if err := root.WriteFile(file.name, file.data, 0600); err != nil {
			return storage.Attachment{}, false, err
		}
	}
	verified, _, err := storage.ReadSessionAttachment(f.opts.StateRoot, sid, rec.ArtifactID)
	return verified, false, err
}

func attachmentSession(t *testing.T, m *modalModel) (*attachmentFixture, *AgentSession, string) {
	t.Helper()
	opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), Principal: "local", Profile: ProfileMemory, Model: m, GenerationFingerprint: "attachment-input-test-v1"}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	snap, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return &attachmentFixture{opts: opts}, s, snap.SessionID
}

func attachmentInput(text string, ids ...string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"text": text, "attachments": ids})
	return raw
}

func TestInputAttachmentsReachModelAsContentBlocks(t *testing.T) {
	m := &modalModel{FakeModel: testkit.NewFake(), image: true}
	c, s, sid := attachmentSession(t, m)
	note, _, err := c.SaveAttachment(t.Context(), sid, attachmentFixtureRequest{IdempotencyKey: "n", MimeType: "text/plain", Name: "notes.txt", Content: []byte("secret-free note body")})
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := c.SaveAttachment(t.Context(), sid, attachmentFixtureRequest{IdempotencyKey: "i", MimeType: "image/png", Content: pngBytes})
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
	img, _, err := c.SaveAttachment(t.Context(), sid, attachmentFixtureRequest{IdempotencyKey: "i", MimeType: "image/png", Content: pngBytes})
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
		{"too many ids", product.CodeInvalidArgument, agent.InputCommand{Kind: "prompt", Content: attachmentInput("x", make([]string, 9)...)}},
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
	other, err := CreateAgentSession(t.Context(), c.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	otherSnap, err := other.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	foreign, _, err := c.SaveAttachment(t.Context(), otherSnap.SessionID, attachmentFixtureRequest{IdempotencyKey: "t", MimeType: "text/plain", Content: []byte("other")})
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

func TestInputAttachmentsTextExpansionPreservesUTF8LimitAndSource(t *testing.T) {
	m := &modalModel{FakeModel: testkit.NewFake()}
	fixture, _, sid := attachmentSession(t, m)
	text := strings.Repeat("a", config.InputAttachmentTextBytes-1) + "汉tail"
	rec, _, err := fixture.SaveAttachment(t.Context(), sid, attachmentFixtureRequest{IdempotencyKey: "text-boundary", MimeType: "text/plain", Name: `quoted"name.txt`, Content: []byte(text)})
	if err != nil {
		t.Fatal(err)
	}
	msg := agent.AgentMessage{Standard: schema.UserAgenticMessage("base"), Attachments: []string{rec.ArtifactID}}
	opts := fixture.opts
	opts.SessionID = sid
	out, err := expandAttachments(opts, []agent.AgentMessage{msg})
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Standard.ContentBlocks) != 1 || len(out[0].Standard.ContentBlocks) != 2 {
		t.Fatal("expansion mutated source or omitted attachment")
	}
	block := out[0].Standard.ContentBlocks[1].UserInputText.Text
	if !utf8.ValidString(block) || !strings.Contains(block, "[attachment truncated]") || strings.Contains(block, "汉tail") || !strings.Contains(block, `name="quoted'name.txt"`) {
		t.Fatal("text expansion violated UTF-8 bound or display-name handling")
	}
	if m.Calls() != 0 {
		t.Fatal("expansion invoked a model")
	}
}
