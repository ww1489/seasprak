package agent

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func userMsg(id, text string) AgentMessage {
	return AgentMessage{ID: id, Kind: KindUser, Status: StatusComplete, Standard: schema.UserAgenticMessage(text)}
}

func callMsg(id, callID string) AgentMessage {
	m := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{{Type: schema.ContentBlockTypeFunctionToolCall, FunctionToolCall: &schema.FunctionToolCall{CallID: callID, Name: "read_file", Arguments: `{}`}}}}
	return AgentMessage{ID: id, Kind: KindAssistant, Status: StatusComplete, Standard: m}
}

func resultMsg(id, callID string) AgentMessage {
	m := &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{{Type: schema.ContentBlockTypeFunctionToolResult, FunctionToolResult: &schema.FunctionToolResult{CallID: callID, Name: "read_file"}}}}
	return AgentMessage{ID: id, Kind: KindToolResult, Status: StatusComplete, Standard: m}
}

func TestPartitionKeepsToolGroupsAndHonoursPreviousSummary(t *testing.T) {
	path := []AgentMessage{userMsg("u1", "a"), callMsg("a1", "c1"), resultMsg("r1", "c1"), userMsg("u2", "b")}
	// keep=2 would cut at r1 (a tool result) or right after a1; both are illegal.
	p, err := PartitionForCompaction(path, 2)
	if err != nil || p.FirstKeptID != "a1" || len(p.H) != 1 || p.H[0].ID != "u1" {
		t.Fatalf("cut split a tool group: %+v %v", p, err)
	}
	// A summary appended after u0,u1 that kept u1 covers only u0.
	summary := AgentMessage{ID: "s1", Kind: KindCompactionSummary, Status: StatusComplete, Summary: &SummaryMessage{Text: "old", FirstKeptID: "u1"}}
	withPrev := []AgentMessage{userMsg("u0", "z"), userMsg("u1", "a"), summary, callMsg("a1", "c1"), resultMsg("r1", "c1"), userMsg("u2", "b")}
	p, err = PartitionForCompaction(withPrev, 1)
	if err != nil || p.Previous == nil || p.Previous.ID != "s1" || p.H[0].ID != "u1" || p.FirstKeptID != "u2" {
		t.Fatalf("previous summary not respected: %+v %v", p, err)
	}
	for _, m := range p.H {
		if m.ID == "u0" {
			t.Fatal("covered entry re-summarized")
		}
	}
	proj := ProjectHistory(withPrev)
	if len(proj) != 5 || proj[0].ID != "s1" || proj[1].ID != "u1" {
		t.Fatalf("projection=%v", proj)
	}
	if _, err = PartitionForCompaction(path, 10); err == nil || !strings.Contains(err.Error(), "no_op") {
		t.Fatal("empty range did not return no_op")
	}
	// keep=1 must move the cut before the whole group, keeping a and r together.
	p, err = PartitionForCompaction([]AgentMessage{userMsg("u", "x"), callMsg("a", "c"), resultMsg("r", "c")}, 1)
	if err != nil || p.FirstKeptID != "a" || len(p.K) != 2 {
		t.Fatalf("group not kept whole: %+v %v", p, err)
	}
	if _, err = PartitionForCompaction([]AgentMessage{callMsg("a", "c"), resultMsg("r", "c")}, 1); err == nil {
		t.Fatal("history that is one tool group produced a cut")
	}
}

func TestValidateSummaryAndAppendix(t *testing.T) {
	good := strings.Join(SummaryHeadings, "\nnone\n") + "\nnone"
	if err := ValidateSummary(good); err != nil {
		t.Fatal(err)
	}
	if ValidateSummary(strings.Replace(good, "## Next Steps", "", 1)) == nil {
		t.Fatal("missing heading accepted")
	}
	if ValidateSummary(good+"\n## Goal") == nil {
		t.Fatal("repeated heading accepted")
	}
	if ValidateSummary(good+"\n"+fileAppendixMarker) == nil {
		t.Fatal("forged appendix marker accepted")
	}
}

func TestMergeFileFactsSequence(t *testing.T) {
	obs := func(status, effect string, executed bool) *ToolObservation {
		return &ToolObservation{Status: status, SideEffect: effect, Executed: executed}
	}
	rec := func(id, name, path string, o *ToolObservation) ToolRecord {
		return ToolRecord{Call: FrozenCall{CallID: id, Name: name, Arguments: `{"path":"` + path + `"}`}, Observation: o}
	}
	calls := []ToolRecord{
		rec("1", "read_file", "a", obs("succeeded", "none", true)),
		rec("2", "read_file", "b", obs("succeeded", "none", true)),
		rec("3", "edit_file", "a", obs("succeeded", "confirmed", true)),
	}
	d := MergeFileFacts(FileDetails{}, FileFactsFromCalls(calls))
	if strings.Join(d.Read, ",") != "b" || strings.Join(d.Modified, ",") != "a" {
		t.Fatalf("first merge=%+v", d)
	}
	more := []ToolRecord{
		rec("4", "write_file", "c", obs("succeeded", "confirmed", true)),
		rec("5", "edit_file", "d", obs("denied", "none", false)),
		rec("6", "write_file", "e", obs("outcome_unknown", "unknown", true)),
		{Call: FrozenCall{CallID: "7", Name: "shell", Arguments: `{"command":"cat f"}`}, Observation: obs("succeeded", "none", true)},
	}
	d = MergeFileFacts(d, FileFactsFromCalls(more))
	if strings.Join(d.Read, ",") != "b" || strings.Join(d.Modified, ",") != "a,c" {
		t.Fatalf("second merge=%+v", d)
	}
	app := FileAppendix(d, 1)
	if !strings.Contains(app, fileAppendixMarker) || !strings.Contains(app, "1 more in details") {
		t.Fatalf("appendix=%q", app)
	}
}

func TestPartitionInTraceSeparatesCompletedPrefix(t *testing.T) {
	inTrace := func(m AgentMessage) AgentMessage { m.Scope.TraceID = "t2"; return m }
	path := []AgentMessage{userMsg("u1", "old"), userMsg("u1b", "older"), inTrace(userMsg("u2", "request")), inTrace(callMsg("a1", "c1")), inTrace(resultMsg("r1", "c1")), inTrace(callMsg("a2", "c2")), inTrace(resultMsg("r2", "c2"))}
	p, err := PartitionForCompactionInTrace(path, 2, "t2")
	if err != nil || len(p.H) != 2 || p.H[1].ID != "u1b" || len(p.P) != 3 || p.P[0].ID != "u2" || p.P[2].ID != "r1" || p.FirstKeptID != "a2" {
		t.Fatalf("H/P/K split wrong: %+v %v", p, err)
	}
	plain, err := PartitionForCompaction(path, 2)
	if err != nil || len(plain.P) != 0 || len(plain.H) != 5 {
		t.Fatalf("idle partition must not produce P: %+v %v", plain, err)
	}
	if ValidateBranchSummary(strings.Join(BranchSummaryHeadings, "\nnone\n")+"\nnone") != nil || ValidateBranchSummary(strings.Join(SummaryHeadings, "\n")) == nil {
		t.Fatal("branch template validation wrong")
	}
	if StripFileAppendix("body"+FileAppendix(FileDetails{Read: []string{"a"}}, 5)) != "body" {
		t.Fatal("appendix not stripped")
	}
}
