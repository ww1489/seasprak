package state_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/sessions/state"
)

type hostCommandConsumer interface{ ConsumeHostCommands(context.Context) error }

func consumeHostCommands(t *testing.T, m *state.Manager) error {
	t.Helper()
	c, ok := any(m).(hostCommandConsumer)
	if !ok {
		t.Fatal("host command context consumer is not implemented")
	}
	return c.ConsumeHostCommands(t.Context())
}
func TestHostCommandConsumptionAtomicAndOrdered(t *testing.T) {
	m, s := fixture(t)
	for _, id := range []string{"z-first", "excluded", "a-last"} {
		if err := m.SaveHostCommand(t.Context(), hostResult(id), id == "excluded"); err != nil {
			t.Fatal(err)
		}
	}
	before := m.View()
	if err := consumeHostCommands(t, m); err != nil {
		t.Fatal(err)
	}
	after := m.View()
	if len(after.Messages) != 2 || after.Messages[0].ID != "z-first" || after.Messages[1].ID != "a-last" || after.LastSeq != before.LastSeq+1 || after.Cursor != before.Cursor || len(after.Events) != len(before.Events) {
		t.Fatal("consumption order, atomicity or display event count differs")
	}
	if !reflect.DeepEqual(before.HostCommands, after.HostCommands) {
		t.Fatal("consumption modified immutable results")
	}
	if err := consumeHostCommands(t, m); err != nil || !reflect.DeepEqual(after, m.View()) {
		t.Fatal("consumption is not idempotent", err)
	}
	reopened, err := state.NewManager(s, "session")
	if err != nil || !reflect.DeepEqual(after, reopened.View()) {
		t.Fatal("consumption lost on replay", err)
	}
	if err := consumeHostCommands(t, reopened); err != nil || !reflect.DeepEqual(after, reopened.View()) {
		t.Fatal("reopen consumed twice", err)
	}
}
