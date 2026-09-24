package sessions

import (
	"encoding/json"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
)

func TestSubscriptionCountsPendingNotLifetime(t *testing.T) {
	sub := &subscription{ch: make(chan agent.Event), wake: make(chan struct{}, 1), done: make(chan struct{}), limits: config.Limits{SubscriptionEvents: 2, SubscriptionBytes: 4096}}
	go sub.deliver()
	defer sub.close(nil)
	for i := 0; i < 100; i++ {
		if !sub.enqueue(agent.Event{Type: "test", Payload: json.RawMessage(`{}`)}) {
			t.Fatalf("subscriber closed after %d delivered events", i)
		}
		select {
		case <-sub.ch:
		case <-time.After(time.Second):
			t.Fatal("event was not delivered")
		}
		deadline := time.Now().Add(time.Second)
		for {
			sub.mu.Lock()
			pending := len(sub.queue)
			sub.mu.Unlock()
			if pending == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("delivered event was not removed")
			}
			goruntime.Gosched()
		}
	}
}
func TestSubscriptionOverflowReportsResync(t *testing.T) {
	sub := &subscription{ch: make(chan agent.Event), wake: make(chan struct{}, 1), done: make(chan struct{}), limits: config.Limits{SubscriptionEvents: 1, SubscriptionBytes: 4096}}
	sub.enqueue(agent.Event{Type: "one", Payload: json.RawMessage(`{}`)})
	if sub.enqueue(agent.Event{Type: "two", Payload: json.RawMessage(`{}`)}) {
		t.Fatal("overflow was accepted")
	}
	public := Subscription{sub: sub}
	pe, ok := product.AsError(public.Err())
	if !ok || pe.Code != product.CodeResyncRequired {
		t.Fatalf("missing resync error: %v", public.Err())
	}
}
