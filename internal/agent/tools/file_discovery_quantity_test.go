package tools

import (
	"fmt"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
	"testing"
)

func TestFileDiscoveryExplicitQuantityLimits(t *testing.T) {
	m := fixture.NewMemory()
	for i := 0; i < 1100; i++ {
		m.SeedFile(fmt.Sprintf("r/%d", i), []byte("hit"))
	}
	for _, tc := range []struct {
		name, args string
		want       int
	}{
		{"ls", `{"root":"r","limit":2}`, 2},
		{"glob", `{"root":"r","pattern":"*","limit":2}`, 2},
		{"grep", `{"root":"r","query":"hit"}`, 100},
	} {
		p := discoveryResult(t, runDiscovery(t, m, tc.name, tc.args))
		if p.Returned != tc.want || !p.Truncated {
			t.Fatalf("%s returned=%d truncated=%v", tc.name, p.Returned, p.Truncated)
		}
		found := false
		for _, reason := range p.Reasons {
			if reason == "entry_limit" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s reasons=%v", tc.name, p.Reasons)
		}
	}
}
