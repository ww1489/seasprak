package agent

import (
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"testing"
)

func TestP2BudgetRestoreDoesNotRefundModelRequests(t *testing.T) {
	b := NewBudget(config.Limits{LogicalModelRequests: 3})
	if err := b.BeginTurn(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := b.OccupyModel(); err != nil {
			t.Fatal(err)
		}
	}
	restored := NewBudget(config.Limits{LogicalModelRequests: 3})
	restored.Restore(b.Snapshot())
	err := restored.OccupyModel()
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeBudgetExhausted {
		t.Fatalf("restored logical call gained a fourth request: %v", err)
	}
	if restored.Snapshot().TransportRequests != 3 {
		t.Fatal("rejected request changed usage")
	}
}
