package sessions

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func registrySession(t *testing.T, root, sid string, main, reviewer *testkit.FakeModel, reviewerVersion string, create bool) *AgentSession {
	t.Helper()
	opts := Options{Workspace: root + "/ws", StateRoot: root + "/state", SessionID: sid, Profile: ProfileMemory, Model: main, Principal: "local", GenerationFingerprint: "agents-v1",
		Agents: []agent.AgentDefinition{{Name: "reviewer", Version: reviewerVersion, Description: "reviews", Instruction: "You review.", Model: reviewer}}}
	var s *AgentSession
	var err error
	if create {
		s, err = CreateAgentSession(t.Context(), opts)
	} else {
		s, err = OpenAgentSession(t.Context(), opts)
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func agentRoots(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{root + "/ws", root + "/state"} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestRegisteredTargetsRouteToTheirOwnAgent(t *testing.T) {
	mainModel, reviewerModel := testkit.NewFake(), testkit.NewFake()
	s := registrySession(t, agentRoots(t), "targets", mainModel, reviewerModel, "r1", true)
	defer s.Close(t.Context())
	in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", TargetAgent: "reviewer", Content: json.RawMessage(`{"text":"check"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if in.TargetAgent.Name != "reviewer" || in.TargetAgent.Version != "r1" {
		t.Fatalf("receipt target %+v", in.TargetAgent)
	}
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	if reviewerModel.Calls() != 1 || mainModel.Calls() != 0 {
		t.Fatalf("reviewer=%d main=%d", reviewerModel.Calls(), mainModel.Calls())
	}
	if tr := s.rt.manager.View().Traces[in.TraceID]; tr.State != "completed" || tr.Target.Name != "reviewer" {
		t.Fatalf("trace %+v", tr)
	}
	prompt(t, s, "default")
	if mainModel.Calls() != 1 || reviewerModel.Calls() != 1 {
		t.Fatalf("default target did not use main: reviewer=%d main=%d", reviewerModel.Calls(), mainModel.Calls())
	}
	caps, err := s.Capabilities(t.Context())
	if err != nil || len(caps.Agents) != 2 || caps.Agents[0].Name != "main" || caps.Agents[1].Name != "reviewer" || caps.Agents[1].Version != "r1" {
		t.Fatalf("capabilities %+v %v", caps, err)
	}
}

func TestUnknownTargetRejectedWithoutWriteOrFallback(t *testing.T) {
	mainModel := testkit.NewFake()
	s := registrySession(t, agentRoots(t), "unknown", mainModel, testkit.NewFake(), "r1", true)
	defer s.Close(t.Context())
	before := s.rt.manager.View().LastSeq
	_, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", TargetAgent: "ghost", Content: json.RawMessage(`{"text":"x"}`)})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeUnsupportedCapability {
		t.Fatalf("unknown target: %v", err)
	}
	if s.rt.manager.View().LastSeq != before || mainModel.Calls() != 0 {
		t.Fatal("unknown target wrote state or fell back to main")
	}
}

func TestDirectedInputInheritsTargetAndRejectsMismatch(t *testing.T) {
	gate := make(chan struct{})
	reviewerModel := testkit.NewFake(testkit.Step{Gate: gate}, testkit.Step{Text: "second"})
	s := registrySession(t, agentRoots(t), "directed", testkit.NewFake(), reviewerModel, "r1", true)
	defer s.Close(t.Context())
	in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", TargetAgent: "reviewer", Content: json.RawMessage(`{"text":"a"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return reviewerModel.Calls() == 1 })
	before := s.rt.manager.View().LastSeq
	_, err = s.SubmitInput(t.Context(), agent.InputCommand{Kind: "follow_up", TargetTraceID: in.TraceID, TargetAgent: "main", Content: json.RawMessage(`{"text":"b"}`)})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict || s.rt.manager.View().LastSeq != before {
		t.Fatalf("mismatched directed target: %v", err)
	}
	_, err = s.SubmitInput(t.Context(), agent.InputCommand{Kind: "chat", TargetAgent: "main", Content: json.RawMessage(`{"text":"b"}`)})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict {
		t.Fatalf("chat to a different target joined the active trace: %v", err)
	}
	follow, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "follow_up", TargetTraceID: in.TraceID, Content: json.RawMessage(`{"text":"b"}`)})
	if err != nil || follow.TargetAgent.Name != "reviewer" || follow.TargetAgent.Version != "r1" {
		t.Fatalf("follow-up did not inherit target: %+v %v", follow, err)
	}
	close(gate)
	waitFor(t, func() bool { return s.rt.manager.View().Traces[in.TraceID].Settled })
	if reviewerModel.Calls() != 2 {
		t.Fatalf("follow-up calls=%d", reviewerModel.Calls())
	}
}

func TestQueuedTargetKeepsAcceptedVersionAcrossReopen(t *testing.T) {
	root := agentRoots(t)
	gate := make(chan struct{})
	mainModel := testkit.NewFake(testkit.Step{Gate: gate})
	s := registrySession(t, root, "reopen", mainModel, testkit.NewFake(), "r1", true)
	busy, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"busy"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return mainModel.Calls() == 1 })
	queued, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", TargetAgent: "reviewer", Content: json.RawMessage(`{"text":"later"}`)})
	if err != nil || queued.State != "pending" {
		t.Fatalf("queued: %+v %v", queued, err)
	}
	if err = s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Reopening with a different implementation version must not replace the
	// accepted target; the queued trace stays browsable and blocked.
	changed := testkit.NewFake()
	s = registrySession(t, root, "reopen", testkit.NewFake(), changed, "r2", false)
	defer s.Close(t.Context())
	if err = s.Cancel(t.Context(), busy.TraceID); err != nil {
		t.Fatal(err)
	}
	before := s.rt.manager.View().LastSeq
	err = s.ContinueQueue(t.Context(), queued.TraceID)
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleResume {
		t.Fatalf("changed version continued: %v", err)
	}
	tr := s.rt.manager.View().Traces[queued.TraceID]
	if changed.Calls() != 0 || s.rt.manager.View().LastSeq != before || tr.State != "queued" || tr.Target.Version != "r1" {
		t.Fatalf("changed version ran or mutated: calls=%d trace=%+v", changed.Calls(), tr)
	}
}

func TestRegistryRejectsInvalidDefinitions(t *testing.T) {
	for name, defs := range map[string][]agent.AgentDefinition{
		"duplicate":    {{Name: "a", Version: "1"}, {Name: "a", Version: "2"}},
		"no-version":   {{Name: "a"}},
		"bad-name":     {{Name: "A B", Version: "1"}},
		"main-version": {{Name: "main", Version: "other"}},
		"workflow-nil": {{Name: "w", Version: "1", Kind: "workflow"}},
	} {
		if _, err := agent.NewAgentRegistry("x", defs); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: testkit.NewFake(), Agents: []agent.AgentDefinition{{Name: "a"}}}
	if _, err := CreateAgentSession(t.Context(), opts); err == nil {
		t.Fatal("invalid registry started a session")
	}
}
