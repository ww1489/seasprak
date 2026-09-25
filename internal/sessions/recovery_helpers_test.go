package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

const (
	windowAccepted    = "input.accepted"
	windowAssistant   = "assistant"
	windowIntent      = "tool_intent"
	windowObservation = "tool_observation"
	windowSettled     = "trace.settled"

	recoveryIdemKey     = "crash-window-key"
	recoveryInstruction = "You are a test agent with the explicitly enabled memory tools."
	crashReadyPrefix    = "SEASPRAK_CRASH_READY "
)

var recoveryPrompt = json.RawMessage(`{"text":"add one"}`)

type crashReport struct {
	Window     string         `json:"window"`
	Phase      string         `json:"phase"`
	ModelCalls int            `json:"modelCalls"`
	ToolCalls  int            `json:"toolCalls"`
	Target     store.Commit   `json:"target"`
	Prefix     []store.Commit `json:"prefix"`
}

type crashStore struct {
	inner     store.Store
	sessionID string
	window    string
	phase     string
	model     *testkit.FakeModel
	effect    string
}

func (s *crashStore) Load(ctx context.Context, sessionID string) (store.StoredSession, error) {
	return s.inner.Load(ctx, sessionID)
}

func (s *crashStore) ReadAfter(ctx context.Context, sessionID string, after uint64) (store.CommitReader, error) {
	return s.inner.ReadAfter(ctx, sessionID, after)
}

func (s *crashStore) Close() error { return s.inner.Close() }

func (s *crashStore) Append(ctx context.Context, sessionID string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	if crashWindowOf(commit) != s.window {
		return s.inner.Append(ctx, sessionID, expected, commit)
	}
	prefix, err := loadPrefix(s.inner, s.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prefix load: %v\n", err)
		os.Exit(6)
	}
	if s.phase == "before" {
		s.hold(commit, prefix)
	}
	receipt, err := s.inner.Append(ctx, sessionID, expected, commit)
	if err != nil {
		return receipt, err
	}
	if s.phase == "after" {
		s.hold(commit, prefix)
	}
	return receipt, err
}

func (s *crashStore) hold(commit store.Commit, prefix []store.Commit) {
	tools, err := readEffect(s.effect)
	if err != nil {
		fmt.Fprintf(os.Stderr, "effect: %v\n", err)
		os.Exit(5)
	}
	raw, err := json.Marshal(crashReport{
		Window:     s.window,
		Phase:      s.phase,
		ModelCalls: s.model.Calls(),
		ToolCalls:  tools,
		Target:     commit,
		Prefix:     prefix,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "report: %v\n", err)
		os.Exit(4)
	}
	if _, err := os.Stdout.Write(append(append([]byte(crashReadyPrefix), raw...), '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "stdout: %v\n", err)
		os.Exit(4)
	}
	_ = os.Stdout.Sync()
	select {}
}

func loadPrefix(backend store.Store, sessionID string) ([]store.Commit, error) {
	loaded, err := backend.Load(context.Background(), sessionID)
	if err != nil {
		return nil, err
	}
	return loaded.Commits, nil
}

func crashWindowOf(c store.Commit) string {
	for _, ev := range c.Events {
		switch ev.Type {
		case "input.accepted":
			return windowAccepted
		case "trace.settled":
			return windowSettled
		}
	}
	if hasToolAssistant(c) {
		return windowAssistant
	}
	var sawClaim, sawObs bool
	for _, r := range c.ControlRecords {
		if r.Type != "tool_call" {
			continue
		}
		var rec agent.ToolRecord
		if json.Unmarshal(r.Payload, &rec) != nil {
			continue
		}
		if rec.Observation != nil {
			sawObs = true
		} else if rec.Claimed {
			sawClaim = true
		}
	}
	if sawObs {
		return windowObservation
	}
	if sawClaim {
		return windowIntent
	}
	return ""
}

func hasToolAssistant(c store.Commit) bool {
	complete := false
	for _, e := range c.Entries {
		if e.Type != "message" {
			continue
		}
		var msg agent.AgentMessage
		if json.Unmarshal(e.Payload, &msg) != nil {
			continue
		}
		if msg.Kind == agent.KindAssistant && msg.Status == agent.StatusComplete {
			complete = true
			if messageHasTools(msg) {
				return true
			}
		}
	}
	if !complete {
		return false
	}
	for _, r := range c.ControlRecords {
		if r.Type == "tool_call" {
			return true
		}
	}
	return false
}

func messageHasTools(msg agent.AgentMessage) bool {
	if msg.Standard == nil {
		return false
	}
	for _, block := range msg.Standard.ContentBlocks {
		if block != nil && block.FunctionToolCall != nil {
			return true
		}
	}
	return false
}

func recoveryTool(effect string) tools.Definition {
	return tools.Definition{
		Name: "add", Version: "v1", Description: "add a number",
		Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
		Run: func(context.Context, json.RawMessage) (string, error) {
			if err := bumpEffect(effect); err != nil {
				return "", err
			}
			return "1", nil
		},
	}
}

func recoveryOpts(ws, stateRoot, sessionID string, model *testkit.FakeModel, effect string) Options {
	return Options{
		Workspace:   ws,
		StateRoot:   stateRoot,
		SessionID:   sessionID,
		Profile:     ProfileMemory,
		Model:       model,
		Instruction: recoveryInstruction,
		Tools:       []tools.Definition{recoveryTool(effect)},
		ToolInfos:   []*schema.ToolInfo{testkit.ToolInfo("add", "add a number")},
	}
}

func bumpEffect(path string) error {
	n, err := readEffect(path)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(strconv.Itoa(n + 1)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func readEffect(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, err
	}
	return n, nil
}

func loadCrashJournal(stateRoot, sessionID string) (store.StoredSession, error) {
	f, err := os.Open(journalPath(stateRoot, sessionID))
	if err != nil {
		return store.StoredSession{}, err
	}
	defer f.Close()
	return jsonl.ReadFile(f, sessionID, 0)
}

func crashWindowPresent(stored store.StoredSession, window string) bool {
	for _, c := range stored.Commits {
		if crashWindowOf(c) == window {
			return true
		}
	}
	return false
}

func countSettled(stored store.StoredSession) int {
	n := 0
	for _, c := range stored.Commits {
		for _, ev := range c.Events {
			if ev.Type == "trace.settled" {
				n++
			}
		}
	}
	return n
}

func commitByID(stored store.StoredSession, id string) (store.Commit, bool) {
	for _, c := range stored.Commits {
		if c.CommitID == id {
			return c, true
		}
	}
	return store.Commit{}, false
}

func acceptedReceipt(stored store.StoredSession) (agent.InputReceipt, bool) {
	return receiptIn(stored.Commits...)
}

func receiptIn(commits ...store.Commit) (agent.InputReceipt, bool) {
	for _, c := range commits {
		for _, ev := range c.Events {
			if ev.Type != "input.accepted" {
				continue
			}
			var rec agent.InputReceipt
			if json.Unmarshal(ev.Payload, &rec) == nil && rec.InputID != "" {
				return rec, true
			}
		}
	}
	return agent.InputReceipt{}, false
}

func expectedCalls(window string) (modelCalls, toolCalls int) {
	switch window {
	case windowAccepted:
		return 0, 0
	case windowAssistant, windowIntent:
		return 1, 0
	case windowObservation:
		return 1, 1
	case windowSettled:
		return 2, 1
	default:
		return -1, -1
	}
}

func expectedUsage(window string) agent.Usage {
	switch window {
	case windowAccepted:
		return agent.Usage{}
	case windowAssistant, windowIntent:
		return agent.Usage{LogicalModelCalls: 1, TransportRequests: 1}
	case windowObservation:
		return agent.Usage{LogicalModelCalls: 1, TransportRequests: 1, ToolExecutions: 1}
	case windowSettled:
		return agent.Usage{LogicalModelCalls: 2, TransportRequests: 2, ToolExecutions: 1}
	default:
		return agent.Usage{LogicalModelCalls: -1}
	}
}

func wantSettled(rep crashReport) int {
	if rep.Window == windowSettled && rep.Phase == "after" {
		return 1
	}
	return 0
}

func persistedCommits(rep crashReport) []store.Commit {
	out := append([]store.Commit(nil), rep.Prefix...)
	if rep.Phase == "after" {
		out = append(out, rep.Target)
	}
	return out
}
