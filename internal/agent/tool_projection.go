package agent

import (
	"encoding/json"
	"unicode/utf8"
)

const processModelContentBytes = 50 << 10

// ToolOutputProjection is presentation attached to an immutable observation.
// It is not execution evidence and cannot replace status or side-effect facts.
type ToolOutputProjection struct {
	CallID      string
	Observation ToolObservation
	Content     string
	Truncated   bool
	Artifact    ArtifactRef
	LogError    string
}

func (p ToolOutputProjection) ModelContent() string {
	return p.ForModel(processModelContentBytes).modelContent()
}

// ForModel returns a presentation copy within the caller's model-envelope
// budget. Stored observations and projections, including artifact identities,
// remain unchanged. Non-process pages retain their continuation semantics.
func (p ToolOutputProjection) ForModel(maxBytes int) ToolOutputProjection {
	if !p.Observation.Process {
		return p
	}
	body := p.modelContent()
	if len(body) <= maxBytes {
		return p
	}
	if p.Artifact.ID != "" {
		fixed := p
		fixed.Content, fixed.Truncated = "", true
		if len(fixed.modelContent()) > maxBytes {
			p.Artifact = ArtifactRef{}
			p.Truncated = true
			p.LogError = "resource_unavailable: process log reference omitted due to model output limit"
		}
	}
	// Recovery can render old observations directly without the Executor.
	// Change only this presentation copy, never the immutable source facts.
	body = p.modelContent()
	text := p.Content
	budget := min(len(text), maxBytes)
	for len(body) > maxBytes && p.Content != "" {
		budget -= max(1, budget/8)
		p.Content, p.Truncated = processModelPreview(text, budget), true
		body = p.modelContent()
	}
	return p
}

func processModelPreview(text string, budget int) string {
	const marker = "\n[truncated]\n"
	if budget <= len(marker) {
		return ""
	}
	remaining := budget - len(marker)
	head, tail := (remaining+1)/2, len(text)-remaining/2
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	for tail < len(text) && !utf8.RuneStart(text[tail]) {
		tail++
	}
	return text[:head] + marker + text[tail:]
}

func (p ToolOutputProjection) modelContent() string {
	if !p.Truncated && p.Artifact.ID == "" && p.LogError == "" {
		return p.Content
	}
	var ref *ArtifactRef
	if p.Artifact.ID != "" {
		ref = &p.Artifact
	}
	body, _ := json.Marshal(struct {
		Content   string       `json:"content"`
		Truncated bool         `json:"truncated,omitempty"`
		Artifact  *ArtifactRef `json:"artifact,omitempty"`
		LogError  string       `json:"logError,omitempty"`
	}{p.Content, p.Truncated, ref, p.LogError})
	return string(body)
}

func (o ToolObservation) ModelContent() string {
	return (ToolOutputProjection{Observation: o, Content: o.Content, Truncated: o.Truncated, LogError: o.LogError}).ModelContent()
}
