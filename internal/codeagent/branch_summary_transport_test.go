package codeagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

// The production factory must meter maintenance requests too. A FakeModel
// cannot reproduce the observed transport's missing-request-identity rejection.
func TestBranchSummaryUsesMeteredObservedTransport(t *testing.T) {
	var summaries, chats atomic.Int32
	var materials []string
	m := p2FactoryModel(t, sessionWire(func(r *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		var request struct {
			Stream   bool              `json:"stream"`
			Messages []json.RawMessage `json:"messages"`
			Tools    []json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		if request.Stream {
			chats.Add(1)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(p2FactorySSE(false))), Request: r}, nil
		}
		summaries.Add(1)
		if len(request.Tools) != 0 {
			t.Error("branch summary was given tools")
		}
		materials = append(materials, string(raw))
		text, _ := json.Marshal(branchText())
		body := `{"id":"branch-summary","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":` + string(text) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "branch-metered", Profile: ProfileMemory, Model: m, Principal: "local", GenerationFingerprint: "branch-metered-v1"}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	prompt(t, s, "shared-root")
	forkPoint := s.rt.manager.View().LeafID
	prompt(t, s, "main-only")
	before := s.rt.manager.View()
	if err := s.ForkBranchWithSummary(t.Context(), "side", forkPoint, true); err != nil {
		t.Fatalf("fork summary: %v summaries=%d", err, summaries.Load())
	}
	forked := s.rt.manager.View()
	if summaries.Load() != 1 || chats.Load() != 2 || !strings.Contains(materials[0], "main-only") || strings.Contains(materials[0], "shared-root") {
		t.Fatal("fork did not summarize only the abandoned suffix exactly once")
	}
	last := forked.Messages[len(forked.Messages)-1]
	if forked.BranchID != "side" || last.Kind != agent.KindBranchSummary || last.Summary == nil || last.Summary.Text != branchText() || forked.Nodes[last.ID].ParentID != forkPoint {
		t.Fatal("fork and generated branch summary were not committed together")
	}
	if !reflect.DeepEqual(before.Traces, forked.Traces) || !reflect.DeepEqual(before.Turns, forked.Turns) || !reflect.DeepEqual(before.ModelAttempts, forked.ModelAttempts) {
		t.Fatal("idle maintenance changed previous trace execution or usage")
	}
	prompt(t, s, "side-only")
	beforeNavigate := s.rt.manager.View()
	if err := s.NavigateBranchWithSummary(t.Context(), "main", true); err != nil {
		t.Fatalf("navigate summary: %v summaries=%d", err, summaries.Load())
	}
	navigated := s.rt.manager.View()
	if summaries.Load() != 2 || chats.Load() != 3 || !strings.Contains(materials[1], "side-only") || strings.Contains(materials[1], "main-only") || strings.Contains(materials[1], "shared-root") {
		t.Fatal("navigation did not summarize its own unique suffix exactly once")
	}
	last = navigated.Messages[len(navigated.Messages)-1]
	if navigated.BranchID != "main" || last.Kind != agent.KindBranchSummary || navigated.LeafID != last.ID || !reflect.DeepEqual(beforeNavigate.Traces, navigated.Traces) {
		t.Fatal("navigation summary or independent maintenance accounting differs")
	}
	if err := s.NavigateBranchWithSummary(t.Context(), "main", true); err != nil || summaries.Load() != 2 {
		t.Fatal("same-branch navigation made an extra request")
	}
	want := projection(t, navigated)
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err = OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection(t, s.rt.manager.View()); got != want || s.rt.manager.View().BranchID != "main" || summaries.Load() != 2 {
		t.Fatal("reopen lost the committed summary or reran maintenance")
	}
}

func TestBranchSummaryObservedFailureKeepsHistory(t *testing.T) {
	for _, scenario := range []string{"invalid-summary", "cancelled", "budget"} {
		t.Run(scenario, func(t *testing.T) {
			var summaries atomic.Int32
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			m := p2FactoryModel(t, sessionWire(func(r *http.Request) (*http.Response, error) {
				raw, _ := io.ReadAll(r.Body)
				var request struct {
					Stream bool `json:"stream"`
				}
				if err := json.Unmarshal(raw, &request); err != nil {
					return nil, err
				}
				if request.Stream {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(p2FactorySSE(false))), Request: r}, nil
				}
				summaries.Add(1)
				text := "## Explored\nincomplete"
				if scenario == "cancelled" {
					cancel()
					text = branchText()
				}
				content, _ := json.Marshal(text)
				body := `{"choices":[{"index":0,"message":{"role":"assistant","content":` + string(content) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			}))
			s, err := CreateAgentSession(t.Context(), Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "branch-failure", Profile: ProfileMemory, Model: m, Principal: "local", GenerationFingerprint: "branch-failure-v1"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			prompt(t, s, "shared")
			forkPoint := s.rt.manager.View().LeafID
			prompt(t, s, "abandoned")
			if scenario == "budget" {
				// No request may start when the independent maintenance budget
				// cannot admit a physical request. Existing trace budgets stay intact.
				s.rt.opts.Limits.TraceTransportRequests = -1
			}
			before := s.rt.manager.View()
			err = s.ForkBranchWithSummary(ctx, "side", forkPoint, true)
			if err == nil {
				t.Fatal("failed maintenance was accepted")
			}
			if scenario == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation rejection: %v", err)
			}
			if scenario == "invalid-summary" {
				var pe *product.Error
				if !errors.As(err, &pe) || pe.Code != product.CodeInvalidArgument {
					t.Fatalf("invalid summary rejection: %v", err)
				}
			}
			wantCalls := int32(1)
			if scenario == "budget" {
				wantCalls = 0
				var pe *product.Error
				if !errors.As(err, &pe) || pe.Code != product.CodeBudgetExhausted {
					t.Fatalf("budget rejection: %v", err)
				}
			}
			if summaries.Load() != wantCalls {
				t.Fatalf("summary requests=%d want=%d", summaries.Load(), wantCalls)
			}
			if !reflect.DeepEqual(before, s.rt.manager.View()) {
				t.Fatal("failed, cancelled or budget-denied summary changed history")
			}
		})
	}
}
