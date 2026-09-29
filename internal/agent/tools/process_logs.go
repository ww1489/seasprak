package tools

import (
	"context"
	"encoding/json"

	"github.com/ww1489/seasprak/internal/agent"
)

func observationOf(out Outcome) *agent.ToolObservation {
	return &agent.ToolObservation{Status: out.Status, Content: out.Content, SideEffect: out.SideEffect, Executed: out.Executed, Process: out.Process, ExitCode: out.ExitCode, Terminated: out.Terminated, ExecutionError: out.ExecutionError, Truncated: out.Truncated, LogError: out.LogError}
}

func outcomeOfRecord(record agent.ToolRecord) Outcome {
	out := outcomeOf(record.Observation)
	if p := record.Projection; p != nil && record.Observation != nil && p.Observation == *record.Observation && p.CallID == record.Call.CallID {
		out.Content, out.Truncated, out.Artifact, out.LogError = p.Content, p.Truncated, p.Artifact, p.LogError
		if out.Process && out.Artifact.ID != "" {
			budget := DefaultOutputLimits().MaxBytes
			if out.Status != "succeeded" {
				// Both representations encode the same content and artifact.
				// Reserve only the extra execution facts in Eino's failure JSON.
				body, _ := json.Marshal(out)
				budget -= max(0, len(body)-len(out.ModelContent()))
			}
			model := p.ForModel(budget)
			out.Content, out.Truncated, out.Artifact, out.LogError = model.Content, model.Truncated, model.Artifact, model.LogError
		}
	}
	return boundProcessOutput(out)
}

// Bound only process previews, measuring the actual model representations rather
// than raw text. File pages have continuation semantics and must not use this.
func boundProcessOutput(out Outcome) Outcome {
	if !out.Process {
		return out
	}
	limits := DefaultOutputLimits()
	text := out.Content
	budget := min(len(text), limits.MaxBytes)
	for {
		size := len(out.ModelContent())
		if out.Status != "succeeded" {
			body, _ := json.Marshal(out) // Eino's failed-result envelope.
			size = max(size, len(body))
		}
		if size <= limits.MaxBytes || out.Content == "" {
			return out
		}
		budget -= max(1, budget/8)
		preview := PreviewHeadTail(text, limits.MaxLines, budget)
		out.Content, out.Truncated = preview.Text, true
	}
}

// Persist execution facts with a safe, bounded fallback before any best-effort
// storage. Unredacted full text is never put into an observation or event.
func (e *Executor) prepareProcessLog(ctx context.Context, out Outcome) (Outcome, string) {
	prepared, full := PrepareOutputLog(ctx, e.operations.OutputRedactor, out.Content)
	out.Content, out.Truncated = prepared.Content, prepared.Truncated
	if prepared.LogError != "" {
		out.LogError = prepared.LogError
	}
	wasTruncated := out.Truncated
	out = boundProcessOutput(out)
	if out.Truncated && !wasTruncated {
		// JSON escaping may require a preview even when the raw log fits.
		// prepared.Content has already passed the redactor; never save an
		// unredacted short log through the optional post-processing port.
		if e.operations.OutputRedactor != nil && prepared.LogError == "" {
			full = prepared.Content
		}
		out.LogError = logNotSaved
		out = boundProcessOutput(out)
	}
	return out, full
}

func (e *Executor) projectProcessLog(ctx context.Context, scope agent.ExecutionScope, call agent.FrozenCall, frozen agent.FrozenExecution, raw Outcome, full string) Outcome {
	out := raw
	if full != "" {
		environment := e.environment
		if environment == "" {
			environment = "trusted-injected"
		}
		input := agent.OutputArtifactInput{Binding: agent.OutputArtifactBinding{SessionID: scope.SessionID, Environment: environment, CallID: call.CallID}, Content: full, MediaType: "text/plain", Name: frozen.Tool + ".log"}
		out.Artifact, out.LogError = SaveOutputLog(ctx, e.operations.Artifacts, input)
	}
	out = boundProcessOutput(out)
	p := agent.ToolOutputProjection{CallID: call.CallID, Observation: *observationOf(raw), Content: out.Content, Truncated: out.Truncated, Artifact: out.Artifact, LogError: out.LogError}
	payload, _ := json.Marshal(p)
	if err := e.sink.CommitFact(context.WithoutCancel(ctx), scope, agent.Fact{Kind: "tool_output_projection", Payload: payload}); err != nil {
		// The original observation remains sufficient to prevent execution replay.
		// A saved but uncommitted reference must not escape to the model/caller.
		out = raw
		out.LogError = logNotSaved
	}
	return boundProcessOutput(out)
}
