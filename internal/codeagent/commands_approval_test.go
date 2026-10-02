package codeagent

import "testing"

// Retained for model approval tests; host shell never uses approval responses.
func answerCommand(t *testing.T, s *AgentSession, decision string) InteractionResponse {
	t.Helper()
	v := s.rt.manager.View()
	for id := range approvalSnapshot(t, s).Interactions {
		r := InteractionResponse{InteractionID: id, Decision: decision, ExpectedRevision: v.LastSeq, IdempotencyKey: "answer"}
		receipt, err := s.RespondInteraction(t.Context(), r)
		if err != nil {
			t.Fatal(err)
		}
		duplicate, err := s.RespondInteraction(t.Context(), r)
		if err != nil || duplicate != receipt {
			t.Fatalf("duplicate answer=%+v err=%v", duplicate, err)
		}
		return r
	}
	t.Fatal("no interaction")
	return InteractionResponse{}
}
