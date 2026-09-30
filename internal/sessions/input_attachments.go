package sessions

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

// inputAttachments returns the attachment IDs referenced by input content.
func inputAttachments(content json.RawMessage) ([]string, error) {
	var body struct {
		Attachments []string `json:"attachments"`
	}
	if json.Unmarshal(content, &body) != nil || len(body.Attachments) == 0 {
		return nil, nil
	}
	if len(body.Attachments) > config.InputAttachments {
		return nil, product.NewError(product.CodeInvalidArgument, "too many attachments in one input")
	}
	seen := map[string]bool{}
	for _, id := range body.Attachments {
		if !validAttachmentID(id) || seen[id] {
			return nil, product.NewError(product.CodeInvalidArgument, "attachment reference is invalid")
		}
		seen[id] = true
	}
	return body.Attachments, nil
}

// modelSupports reports a declared or verified modality of a catalog model.
// Test doubles without a configuration accept text only.
func modelSupports(m model.AgenticModel, name llm.CapabilityName) bool {
	configured, ok := m.(interface{ Configuration() llm.ModelConfig })
	if !ok {
		return false
	}
	status := configured.Configuration().Capabilities.Capability(name).Status
	return status == llm.Declared || status == llm.Verified
}

// admitAttachments runs at acceptance. Every reference must resolve inside
// this session and match a modality of the model that will execute the
// target; otherwise nothing is written and no model is called.
func (rt *runtime) admitAttachments(cmd agent.InputCommand, target agent.TargetAgent) error {
	ids, err := inputAttachments(cmd.Content)
	if err != nil || len(ids) == 0 {
		return err
	}
	// Steering is merged into a running request without the projection path
	// that resolves attachments, so it cannot carry them.
	if cmd.Kind == "steering" {
		return product.NewError(product.CodeUnsupportedCapability, "steering input cannot reference attachments")
	}
	if rt.opts.StateRoot == "" || rt.opts.StateRoot == "memory" {
		return product.NewError(product.CodeUnsupportedCapability, "attachments require a durable session")
	}
	if def, err := rt.definitionFor(target); err == nil && def.Kind == agent.AgentKindWorkflow {
		return product.NewError(product.CodeUnsupportedCapability, "workflow inputs cannot reference attachments")
	}
	m := rt.opts.Model
	if def, err := rt.definitionFor(target); err == nil && def.Model != nil {
		m = def.Model
	}
	for _, id := range ids {
		rec, _, err := readSessionAttachment(rt.opts.StateRoot, rt.opts.SessionID, id)
		if err != nil {
			return err
		}
		if isImage(rec.MimeType) && !modelSupports(m, llm.CapInputImage) {
			return product.NewError(product.CodeUnsupportedCapability, "model does not accept image input")
		}
	}
	return nil
}

func isImage(mime string) bool { return mime == "image/png" || mime == "image/jpeg" }

// expandAttachments resolves attachment references of projected messages
// into model content blocks. The history keeps only the IDs.
func expandAttachments(stateRoot, sid string, msgs []agent.AgentMessage) ([]agent.AgentMessage, error) {
	out := make([]agent.AgentMessage, len(msgs))
	copy(out, msgs)
	for i, m := range out {
		if len(m.Attachments) == 0 || m.Standard == nil {
			continue
		}
		copied := *m.Standard
		copied.ContentBlocks = append([]*schema.ContentBlock(nil), m.Standard.ContentBlocks...)
		for _, id := range m.Attachments {
			rec, data, err := readSessionAttachment(stateRoot, sid, id)
			if err != nil {
				return nil, product.NewError(product.CodeStorageUnavailable, "referenced attachment is unavailable")
			}
			if isImage(rec.MimeType) {
				copied.ContentBlocks = append(copied.ContentBlocks, schema.NewContentBlock(&schema.UserInputImage{Base64Data: base64.StdEncoding.EncodeToString(data), MIMEType: rec.MimeType}))
				continue
			}
			text := string(data)
			if len(text) > config.InputAttachmentTextBytes {
				cut := config.InputAttachmentTextBytes
				for cut > 0 && !utf8.RuneStart(text[cut]) {
					cut--
				}
				text = text[:cut] + "\n[attachment truncated]"
			}
			name := strings.ReplaceAll(rec.Name, `"`, "'")
			if name == "" {
				name = rec.ArtifactID
			}
			copied.ContentBlocks = append(copied.ContentBlocks, schema.NewContentBlock(&schema.UserInputText{Text: "<attachment name=\"" + name + "\" type=\"" + rec.MimeType + "\">\n" + text + "\n</attachment>"}))
		}
		out[i].Standard = &copied
	}
	return out, nil
}
