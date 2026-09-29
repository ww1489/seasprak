package agenticgemini

import (
	"context"

	"google.golang.org/genai"
)

// ResponseRestorer restores only function arguments lost by the pinned SDK's
// float64 decoding. A factory creates one instance per Generate/Stream call.
// It must reject missing or conflicting associations, never guess identities.
type ResponseRestorer interface {
	Restore(*genai.GenerateContentResponse) error
	Close()
}

type ResponseRestorerFactory func(context.Context) (context.Context, ResponseRestorer)
