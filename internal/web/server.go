package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/sessions"
)

// Server owns the listener, the per-instance credential file and, when no test
// handler is injected, the session catalog behind the /v1 routes.
type Server struct {
	url, tokenPath string
	options        sessions.Options
	cancel         context.CancelFunc
	done           chan struct{}
	err            error // Written before done closes; read only by Wait.
}

func (s *Server) URL() string                      { return s.url }
func (s *Server) TokenPath() string                { return s.tokenPath }
func (s *Server) SessionOptions() sessions.Options { return s.options }
func (s *Server) Close()                           { s.cancel() }
func (s *Server) Wait() error                      { <-s.done; return s.err }

// testOptions is set only by this package's tests to substitute an offline
// model after trusted startup parsing. Production code never assigns it.
var testOptions func(*sessions.Options)

// unusedConns tracks connections that have not started a request.
type unusedConns struct {
	mu    sync.Mutex
	fresh map[net.Conn]struct{}
}

func (u *unusedConns) track(c net.Conn, state http.ConnState) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if state == http.StateNew {
		u.fresh[c] = struct{}{}
	} else {
		delete(u.fresh, c)
	}
}

// closeUntil repeatedly closes connections still in StateNew until done.
func (u *unusedConns) closeUntil(done <-chan struct{}) {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		u.mu.Lock()
		for c := range u.fresh {
			_ = c.Close()
			delete(u.fresh, c)
		}
		u.mu.Unlock()
		select {
		case <-done:
			return
		case <-tick.C:
		}
	}
}

func Start(parent context.Context, c Config, handler http.Handler) (*Server, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	addr, err := listenAddress(c.Listen)
	if err != nil {
		return nil, err
	}
	options, err := loadOptions(parent, c)
	if err != nil {
		return nil, err
	}
	if testOptions != nil {
		testOptions(&options)
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return nil, unavailable("credential generation failed")
	}
	token := base64.RawURLEncoding.EncodeToString(secret[:])
	path := filepath.Join(options.StateRoot, "web-"+rand.Text()+".token")
	f, err := privateFile(path)
	if err != nil {
		return nil, unavailable("credential file creation failed")
	}
	_, writeErr := io.WriteString(f, token)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return nil, unavailable("credential file publication failed")
	}
	if err = checkPrivate(path); err != nil {
		_ = os.Remove(path)
		return nil, unavailable("credential file protection failed")
	}
	lc := net.ListenConfig{}
	listener, err := lc.Listen(parent, "tcp", addr)
	if err != nil {
		_ = os.Remove(path)
		return nil, unavailable("loopback listener unavailable")
	}
	if err = parent.Err(); err != nil {
		listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	// A nil handler assembles the real session routes over one catalog that owns
	// the state root's creation registry for the lifetime of the service.
	var catalog *sessions.Catalog
	if handler == nil {
		catalog, err = sessions.NewCatalog(options)
		if err != nil {
			listener.Close()
			_ = os.Remove(path)
			return nil, unavailable("session catalog unavailable")
		}
		handler = newRoutes(catalog, localPrincipal)
	}
	ctx, cancel := context.WithCancel(parent)
	s := &Server{url: "http://" + listener.Addr().String(), tokenPath: path, options: options, cancel: cancel, done: make(chan struct{})}
	conns := &unusedConns{fresh: map[net.Conn]struct{}{}}
	server := &http.Server{
		ConnState:         conns.track,
		Handler:           authorize(listener.Addr().String(), token, handler),
		ReadHeaderTimeout: config.WebReadHeaderTimeout, IdleTimeout: config.WebIdleTimeout, MaxHeaderBytes: config.WebHeaderBytes,
		// SSE will use per-write deadlines; there is deliberately no total WriteTimeout.
		BaseContext: func(net.Listener) context.Context { return ctx },
		// net/http panic diagnostics can include handler values; never log raw errors.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	go func() {
		defer close(s.done)
		defer cancel()
		select {
		case err := <-served:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.err = unavailable("HTTP service stopped unexpectedly")
			}
			_ = server.Close()
		case <-ctx.Done():
			shutdownCtx, stop := context.WithTimeout(context.Background(), config.WebShutdownTimeout)
			// net/http counts a connection that has not sent a request as
			// active for its first seconds; close those so an unused preconnect
			// cannot hold shutdown until the deadline. Nothing was handled on them.
			closing := make(chan struct{})
			go conns.closeUntil(closing)
			err := server.Shutdown(shutdownCtx)
			close(closing)
			stop()
			if err != nil {
				_ = server.Close()
				s.err = unavailable("HTTP shutdown did not complete within its deadline")
			}
			serveErr := <-served
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				s.err = unavailable("HTTP service stopped unexpectedly")
			}
		}
		if catalog != nil {
			closeCtx, stop := context.WithTimeout(context.Background(), config.WebShutdownTimeout)
			if err := catalog.Close(closeCtx); err != nil && s.err == nil {
				s.err = unavailable("session shutdown did not complete")
			}
			stop()
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			s.err = unavailable("credential file cleanup failed")
		}
	}()
	return s, nil
}
