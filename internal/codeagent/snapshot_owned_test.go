package codeagent

import (
	"context"
	"encoding/json"
	"reflect"
	goruntime "runtime"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
)

func TestSnapshotMessagesConcurrentIsolation(t *testing.T) {
	f := newSnapshotBenchmarkFixture(t, 1, "completed")
	before := f.s.rt.manager.View()
	owned, err := f.s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, msg := range owned.Messages {
				if msg.Standard == nil {
					continue
				}
				for _, block := range msg.Standard.ContentBlocks {
					if block.Reasoning != nil {
						block.Reasoning.Text = "mutated"
						block.Reasoning.Signature = "mutated"
					}
					if block.AssistantGenText != nil {
						block.AssistantGenText.Text = "mutated"
					}
				}
				if msg.Standard.ResponseMeta != nil && msg.Standard.ResponseMeta.TokenUsage != nil {
					msg.Standard.ResponseMeta.TokenUsage.TotalTokens = 999
				}
			}
			goruntime.Gosched()
		}
	}()
	defer func() { close(stop); <-done }()
	for i := 0; i < 10; i++ {
		fresh, err := f.s.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range fresh.Messages {
			if msg.Standard == nil {
				continue
			}
			for _, b := range msg.Standard.ContentBlocks {
				if b.Reasoning != nil && b.Reasoning.Text == "mutated" {
					t.Fatal("reasoning alias")
				}
				if b.AssistantGenText != nil && b.AssistantGenText.Text == "mutated" {
					t.Fatal("text alias")
				}
			}
		}
	}
	if !reflect.DeepEqual(before, f.s.rt.manager.View()) {
		t.Fatal("snapshot mutation reached manager")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.s.Snapshot(ctx); err == nil {
		t.Fatal("cancelled snapshot succeeded")
	}
	if err := f.s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	closed, err := f.s.Snapshot(t.Context())
	if err != nil || closed.Resume != nil {
		t.Fatalf("closed snapshot: %v", err)
	}
	// Comparing JSON avoids map iteration order and confirms closed projection is
	// still identical to a fresh public projection of private committed history.
	got, _ := json.Marshal(closed.Messages)
	wantMessages := make([]agent.AgentMessage, 0, len(before.Messages))
	for _, msg := range before.Messages {
		wantMessages = append(wantMessages, agent.PublicMessage(msg))
	}
	want, _ := json.Marshal(wantMessages)
	if string(got) != string(want) {
		t.Fatal("closed snapshot changed public messages")
	}
}

type snapshotProjectionProbe struct{ calls int }

func (p *snapshotProjectionProbe) MarshalJSON() ([]byte, error) {
	p.calls++
	return []byte(`"synthetic-private-probe"`), nil
}

// The caller has already obtained an isolated View. Display must consume its
// owned messages, not serialize them again. Both selection branches matter.
func TestSnapshotOwnedProjectionDoesNotSerializeAgain(t *testing.T) {
	for _, host := range []bool{false, true} {
		name := "history"
		if host {
			name = "host"
		}
		t.Run(name, func(t *testing.T) {
			probe := &snapshotProjectionProbe{}
			msg := agent.AgentMessage{ID: "owned", Standard: privateReplayFixture()}
			msg.Standard.Extra["probe"] = probe
			view := state.View{Messages: []agent.AgentMessage{msg}}
			if host {
				view.HostCommands = map[string]state.HostCommandResult{"owned": {Message: msg}}
			}
			out := snapshotMessages(view)
			if len(out) != 1 || probe.calls != 0 {
				t.Fatalf("messages=%d repeated serializations=%d", len(out), probe.calls)
			}
			if out[0].Standard != msg.Standard {
				t.Fatal("owned standard message was copied again")
			}
			assertNoPrivateReplay(t, "owned projection", out)
		})
	}
}
