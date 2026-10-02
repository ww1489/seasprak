package web

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/codeagent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestServerWaitRetainsRegistryUntilNoncooperativeWriterExit(t *testing.T) {
	conf := testConfig(t)
	setProfile(t, conf, codeagent.ProfileMemory)
	gate := make(chan struct{})
	model := &catalogUncooperativeModel{FakeModel: testkit.NewFake(testkit.Step{Gate: gate, Text: "done"}), started: make(chan struct{}), cancelled: make(chan struct{})}
	testOptions = func(o *codeagent.Options) { o.Model = model }
	server, err := Start(t.Context(), conf, nil)
	testOptions = nil
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
		server.Close()
		server.Wait()
	})
	raw, err := os.ReadFile(server.TokenPath())
	if err != nil {
		t.Fatal("credential fixture unavailable")
	}
	sid := createSession(t, server, string(raw), conf)
	status, _ := call(t, server, string(raw), "POST", "/v1/sessions/"+sid+"/inputs", "wait", map[string]any{"kind": "prompt", "content": []map[string]string{{"type": "text", "text": "wait for actual exit"}}})
	if status != http.StatusAccepted {
		t.Fatalf("input status=%d", status)
	}
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	server.Close()
	waited := make(chan error, 1)
	go func() { waited <- server.Wait() }()
	select {
	case <-model.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not cancel session")
	}
	// The shutdown deadline is allowed to report a timeout, but completion and
	// registry release must still wait for the noncooperative runner to exit.
	var early error
	returned := false
	select {
	case early = <-waited:
		returned = true
		t.Error("server Wait reported completion before writer exit")
	case <-time.After(config.WebShutdownTimeout + 250*time.Millisecond):
	}
	if other, err := store.OpenCreationRegistry(server.SessionOptions().StateRoot); err == nil {
		other.Close()
		t.Error("server released registry before writer exit")
	}
	close(gate)
	if !returned {
		select {
		case early = <-waited:
		case <-time.After(5 * time.Second):
			t.Fatal("server did not finish after actual exit")
		}
	}
	if pe, ok := product.AsError(early); !ok || pe.Code != product.CodeResourceUnavailable {
		t.Errorf("server shutdown timeout error=%v", early)
	}
	if model.Calls() != 1 {
		t.Fatalf("model calls=%d", model.Calls())
	}
	other, err := store.OpenCreationRegistry(server.SessionOptions().StateRoot)
	if err != nil {
		t.Fatal("server retained registry after actual writer exit")
	}
	defer other.Close()
	opts := server.SessionOptions()
	opts.SessionID = sid
	session, err := codeagent.OpenAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal("server retained session writer after actual exit")
	}
	if err = session.Close(t.Context()); err != nil {
		if pe, ok := product.AsError(err); !ok {
			t.Fatal(err)
		} else {
			t.Fatalf("reopened close=%s", pe.Code)
		}
	}
}
