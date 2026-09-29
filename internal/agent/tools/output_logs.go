package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

const logNotSaved = "resource_unavailable: full process log was not saved"

// PreparedOutputLog contains only a safe, bounded display of process output.
// It carries no execution facts, permissions or model call identity.
type PreparedOutputLog struct {
	Content   string
	Truncated bool
	LogError  string
}

// PrepareOutputLog redacts output and builds its head/tail preview. The separate
// full return is redacted text to save only when the preview is truncated. A
// missing redactor preserves trusted short output but hides oversized output.
func PrepareOutputLog(ctx context.Context, redactor agent.OutputRedactor, text string) (prepared PreparedOutputLog, full string) {
	limits := DefaultOutputLimits()
	original := PreviewHeadTail(text, limits.MaxLines, limits.MaxBytes)
	defer func() {
		if recover() != nil {
			prepared.Content = "[process output omitted: redaction failed]"
			prepared.Truncated = original.Truncated
			prepared.LogError = logNotSaved
			full = ""
		}
	}()
	if redactor != nil {
		var err error
		text, err = redactor(ctx, text)
		if err != nil || !utf8.ValidString(text) {
			prepared.Content = "[process output omitted: redaction failed]"
			prepared.Truncated = original.Truncated
			prepared.LogError = logNotSaved
			return prepared, ""
		}
	} else if original.Truncated {
		prepared.Content = "[process output omitted: redactor unavailable]"
		prepared.Truncated = true
		prepared.LogError = logNotSaved
		return prepared, ""
	}
	preview := PreviewHeadTail(text, limits.MaxLines, limits.MaxBytes)
	prepared.Content, prepared.Truncated = preview.Text, preview.Truncated
	if preview.Truncated {
		prepared.LogError = logNotSaved
		return prepared, text
	}
	return prepared, ""
}

// SaveOutputLog attempts one save through the optional trusted post-processing
// port. Callers supply redacted full output and a host-established binding; no
// execution ticket is used. A failed or inconsistent save never publishes a ref.
func SaveOutputLog(ctx context.Context, artifacts agent.ArtifactStore, input agent.OutputArtifactInput) (ref agent.ArtifactRef, logError string) {
	logError = logNotSaved
	defer func() {
		if recover() != nil {
			ref = agent.ArtifactRef{}
			logError = logNotSaved
		}
	}()
	store, ok := artifacts.(agent.OutputArtifactStore)
	if !ok {
		return agent.ArtifactRef{}, logError
	}
	result, err := store.SaveOutput(ctx, input)
	if err != nil {
		return agent.ArtifactRef{}, logError
	}
	sum := sha256.Sum256([]byte(input.Content))
	if result.ID == "" || !result.Available || result.SessionID != input.Binding.SessionID || result.Environment != input.Binding.Environment || result.Size != int64(len(input.Content)) || result.Hash != hex.EncodeToString(sum[:]) {
		return agent.ArtifactRef{}, product.CodeStateConflict + ": saved process log reference does not match output"
	}
	// Reserve space for the fixed process-result envelope as well as the
	// reference. The store was called once; an unpublishable reference is not
	// proof that saving did not happen, and its identity must not be cut.
	const envelopeReserve = 1024
	encoded, _ := json.Marshal(result)
	if len(encoded) > DefaultOutputLimits().MaxBytes-envelopeReserve {
		return agent.ArtifactRef{}, product.CodeResourceUnavailable + ": saved process log reference exceeds model output limit"
	}
	return result, ""
}
