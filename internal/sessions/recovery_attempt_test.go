package sessions

import (
	"github.com/ww1489/seasprak/internal/sessions/store"
	"testing"
)

func TestCrashWindowRecognizesModelAttempt(t *testing.T) {
	if got := crashWindowOf(store.Commit{ControlRecords: []store.Record{{Type: "model_attempt"}}}); got != "model_attempt" {
		t.Fatalf("attempt commit has no crash barrier: %q", got)
	}
}
