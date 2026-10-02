package web

import (
	"errors"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
)

func TestServerWaitPreservesResourceCleanupError(t *testing.T) {
	conf := testConfig(t)
	configureHTTPWorkflow(t, conf, httpLiteralWorkflow())
	server, _, _ := workflowHTTPServer(t, conf)
	cleanupErr := errors.New("fixture resource cleanup failed")
	exitedCatalogErrorSession(t, server.catalog, cleanupErr)
	server.Close()
	err := server.Wait()
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeResourceUnavailable {
		t.Fatalf("Server.Wait changed its public typed error: %v", err)
	}
	if !errors.Is(server.closeErr, cleanupErr) {
		t.Fatalf("Server lost original cleanup error: %v", server.closeErr)
	}
}
