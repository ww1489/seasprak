package tools

import (
	"context"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
	"testing"
)

type cancelledDiscovery struct {
	*fixture.Memory
	cancel context.CancelFunc
}

func (b cancelledDiscovery) List(ctx context.Context, r agent.ListRequest) (agent.ListResult, error) {
	out, err := b.Memory.List(ctx, r)
	b.cancel()
	return out, err
}
func TestFileDiscoveryCancellationAfterBackend(t *testing.T) {
	m := fixture.NewMemory()
	m.SeedFile("r/a", []byte("body"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	def := builtinByName(t, NewBuiltinDefinitions(BuiltinOptions{}), "ls")
	args := `{"root":"r"}`
	e, err := NewExecutor("g", []Definition{def}, &recordSink{found: true, rec: builtinAccepted(args, "ls", "provider")}, allow{}, agent.NewBudget(config.DefaultLimits()), WithOperations(Operations{Files: cancelledDiscovery{m, cancel}}), WithResourceScheduler(NewResourceScheduler()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.Run(ctx, agent.ExecutionScope{}, "provider", "ls", args)
	if err != nil || out.Status != "cancelled" || !out.Executed || out.SideEffect != "none" || m.Calls("list") != 1 {
		t.Fatalf("out=%+v err=%v calls=%d", out, err, m.Calls("list"))
	}
}
