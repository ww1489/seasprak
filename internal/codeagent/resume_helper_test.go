package codeagent

import "testing"

// Ordinary checkpoint and delegated-child recovery use the real session entry.
func resumeSessionTrace(t *testing.T, s *AgentSession, traceID string) {
	t.Helper()
	if _, err := s.Resume(t.Context(), ResumeCommand{TraceID: traceID, ExpectedRevision: s.rt.manager.View().LastSeq}); err != nil {
		t.Fatal(err)
	}
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	requireSessionCode(t, err, code)
}
