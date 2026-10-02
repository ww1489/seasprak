package codeagent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func collectDurable(t *testing.T, r *ReplaySubscription, until func([]uint64) bool) []uint64 {
	t.Helper()
	var seqs []uint64
	timeout := time.After(5 * time.Second)
	for !until(seqs) {
		select {
		case ev, ok := <-r.Events:
			if !ok {
				t.Fatalf("replay closed early: %v", r.Err())
			}
			if ev.DurableSeq != nil {
				seqs = append(seqs, *ev.DurableSeq)
			}
		case <-timeout:
			t.Fatalf("replay stalled at %v", seqs)
		}
	}
	return seqs
}

func TestSubscribeFromReplaysHistoryThenLiveWithoutGap(t *testing.T) {
	gate := make(chan struct{})
	s, manager, model := controlSession(t, "replay", testkit.Step{Text: "one"}, testkit.Step{Gate: gate, Text: "two"})
	first, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"a"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return manager.View().Traces[first.TraceID].Settled })
	if _, err = s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"b"}`)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return model.Calls() == 2 })
	r, err := s.SubscribeFrom(t.Context(), 1, config.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	handoff := r.Handoff
	close(gate)
	var final uint64
	waitFor(t, func() bool {
		v := manager.View()
		final = v.Cursor
		for _, tr := range v.Traces {
			if !tr.Settled {
				return false
			}
		}
		return final > handoff
	})
	seqs := collectDurable(t, r, func(s []uint64) bool { return len(s) > 0 && s[len(s)-1] >= final })
	for i, seq := range seqs {
		if seq != uint64(i)+2 {
			t.Fatalf("gap or duplicate at %d: %v (handoff %d)", i, seqs, handoff)
		}
	}
}

func TestSubscribeFromRejectsFutureCursorAndCleansUp(t *testing.T) {
	s, _, _ := controlSession(t, "replay-bad")
	if _, err := s.SubscribeFrom(t.Context(), 99, config.Limits{}); err == nil {
		t.Fatal("future cursor accepted")
	} else if pe, _ := product.AsError(err); pe.Code != product.CodeInvalidArgument {
		t.Fatalf("code=%s", pe.Code)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.SubscribeFrom(ctx, 0, config.Limits{}); err == nil {
		t.Fatal("cancelled registration succeeded")
	}
	count := 0
	_ = s.rt.do(t.Context(), func(rt *runtime) error { count = len(rt.subs); return nil })
	if count != 0 {
		t.Fatalf("leaked %d subscriptions", count)
	}
	r, err := s.SubscribeFrom(t.Context(), 0, config.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	_ = s.rt.do(t.Context(), func(rt *runtime) error { count = len(rt.subs); return nil })
	if count != 0 {
		t.Fatal("closed replay kept its live subscription")
	}
}

func TestSubscribeFromOverflowReportsResync(t *testing.T) {
	s, manager, _ := controlSession(t, "replay-overflow")
	for i := 0; i < 3; i++ {
		in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"x"}`)})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return manager.View().Traces[in.TraceID].Settled })
	}
	r, err := s.SubscribeFrom(t.Context(), 0, config.Limits{SubscriptionEvents: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Do not read: new live events overflow the one-slot buffer.
	for i := 0; i < 2; i++ {
		if _, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"y"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return r.live.Err() != nil })
	for range r.Events {
	}
	if pe, ok := product.AsError(r.Err()); !ok || pe.Code != product.CodeResyncRequired {
		t.Fatalf("err=%v", r.Err())
	}
}
