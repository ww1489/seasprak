package web

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/codeagent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

// The gate controls a real publication, not an alternative file implementation.
// completed includes only successful publishFile calls, even when a simulated
// post-publication error makes the caller uncertain of the published outcome.
type catalogPublicationGate struct {
	mu             sync.Mutex
	started        chan struct{}
	release        chan struct{}
	attempts       []string
	completed      []string
	postError      error
	badRecordOrder bool
}

func newCatalogPublicationGate(t *testing.T) *catalogPublicationGate {
	t.Helper()
	g := &catalogPublicationGate{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.unblock)
	return g
}
func (g *catalogPublicationGate) unblock() {
	select {
	case <-g.release:
	default:
		close(g.release)
	}
}
func (g *catalogPublicationGate) publish(root *os.Root, dir, name string, data []byte) error {
	g.mu.Lock()
	g.attempts = append(g.attempts, name)
	first := len(g.attempts) == 1
	g.mu.Unlock()
	if first {
		close(g.started)
		<-g.release
	}
	if id, ok := strings.CutSuffix(name, ".json"); ok && store.ValidAttachmentID(id) {
		f, err := root.Open(id + ".bin")
		if err == nil {
			err = f.Close()
		}
		g.mu.Lock()
		g.badRecordOrder = g.badRecordOrder || err != nil || len(g.completed) != 1 || g.completed[0] != id+".bin"
		g.mu.Unlock()
	}
	if err := publishFile(root, dir, name, data); err != nil {
		return err
	}
	g.mu.Lock()
	g.completed = append(g.completed, name)
	g.mu.Unlock()
	return g.postError
}
func (g *catalogPublicationGate) counts() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.attempts), len(g.completed)
}
func waitPublication(t *testing.T, g *catalogPublicationGate) {
	t.Helper()
	select {
	case <-g.started:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not reach its controlled gate")
	}
}
func waitFileWriterQueued(t *testing.T, method string) {
	t.Helper()
	// Observe the actual mutex wait instead of assuming a goroutine has
	// completed permission/parameter validation after an arbitrary sleep.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(stack, "(*Catalog)."+method+"(") && strings.Contains(stack, "sync.(*Mutex).Lock") {
				return
			}
		}
		runtime.Gosched()
	}
	t.Fatal("file writer did not queue at the files mutex")
}
func fileMutation(c *Catalog, ctx context.Context, sid, kind, key string) error {
	if kind == "attachment" {
		_, _, err := c.SaveAttachment(ctx, sid, SaveAttachmentRequest{IdempotencyKey: key, MimeType: "text/plain", Name: "pending.txt", Content: []byte("complete bytes")})
		return err
	}
	_, _, err := c.SetMetadata(ctx, sid, SetMetadataRequest{IdempotencyKey: key, Name: "pending", Labels: []string{"x"}})
	return err
}

func TestCatalogQueuedFileWritesRejectCloseWithoutPublication(t *testing.T) {
	for _, kind := range []string{"attachment", "metadata"} {
		t.Run(kind, func(t *testing.T) {
			c, opts, sid := attachmentCatalog(t)
			g := newCatalogPublicationGate(t)
			g.unblock()
			c.publish = g.publish
			c.files.Lock()
			locked := true
			defer func() {
				if locked {
					c.files.Unlock()
				}
			}()
			written := make(chan error, 1)
			go func() { written <- fileMutation(c, t.Context(), sid, kind, "queued") }()
			method := "SaveAttachment"
			if kind == "metadata" {
				method = "SetMetadata"
			}
			waitFileWriterQueued(t, method)
			if err := c.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			other, err := NewCatalog(opts)
			if err != nil {
				t.Fatal("unadmitted queued write retained registry")
			}
			defer other.Close(context.Background())
			c.files.Unlock()
			locked = false
			err = <-written
			if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict {
				t.Errorf("queued write after close=%v, want state_conflict", err)
			}
			if a, p := g.counts(); a != 0 || p != 0 {
				t.Errorf("unadmitted publication attempts/completed=%d/%d", a, p)
			}
			name := "attachments"
			if kind == "metadata" {
				name = metadataName
			}
			if _, err := os.Lstat(filepath.Join(opts.StateRoot, "sessions", sid, name)); !os.IsNotExist(err) {
				t.Errorf("queued old owner created %s: %v", name, err)
			}
		})
	}
}

func TestCatalogQueuedFileWritesCancelBeforeAdmission(t *testing.T) {
	for _, kind := range []string{"attachment", "metadata"} {
		t.Run(kind, func(t *testing.T) {
			c, opts, sid := attachmentCatalog(t)
			g := newCatalogPublicationGate(t)
			g.unblock()
			c.publish = g.publish
			c.files.Lock()
			locked := true
			defer func() {
				if locked {
					c.files.Unlock()
				}
			}()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			written := make(chan error, 1)
			go func() { written <- fileMutation(c, ctx, sid, kind, "cancelled") }()
			method := "SaveAttachment"
			if kind == "metadata" {
				method = "SetMetadata"
			}
			waitFileWriterQueued(t, method)
			cancel()
			c.files.Unlock()
			locked = false
			if err := <-written; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel before admission=%v", err)
			}
			if a, p := g.counts(); a != 0 || p != 0 {
				t.Errorf("cancelled unadmitted publications=%d/%d", a, p)
			}
			c.mu.Lock()
			active := c.activeFileWrites
			c.mu.Unlock()
			if active != 0 {
				t.Errorf("cancelled unadmitted count=%d", active)
			}
			name := attachmentsDir
			if kind == "metadata" {
				name = metadataName
			}
			if _, err := os.Lstat(filepath.Join(opts.StateRoot, "sessions", sid, name)); !os.IsNotExist(err) {
				t.Errorf("cancelled unadmitted write created %s: %v", name, err)
			}
		})
	}
}

func TestCatalogAdmittedFileWritesRetainRegistryUntilPublicationExit(t *testing.T) {
	for _, kind := range []string{"attachment", "metadata"} {
		for _, outcome := range []string{"success", "post_publication_error", "cancelled_context"} {
			t.Run(kind+"/"+outcome, func(t *testing.T) {
				c, opts, sid := attachmentCatalog(t)
				dir := filepath.Join(opts.StateRoot, "sessions", sid)
				if kind == "metadata" {
					for _, req := range []SetMetadataRequest{{IdempotencyKey: "original", Name: "one"}, {IdempotencyKey: "second", Name: "two"}} {
						if _, _, err := c.SetMetadata(t.Context(), sid, req); err != nil {
							t.Fatal(err)
						}
					}
				} else if outcome == "success" {
					for i := 0; i < config.WebAttachmentsPerSession-1; i++ {
						if err := fileMutation(c, t.Context(), sid, kind, "seed-"+strconv.Itoa(i)); err != nil {
							t.Fatal(err)
						}
					}
				}
				journal := filepath.Join(dir, "journal.jsonl")
				before, err := os.ReadFile(journal)
				if err != nil {
					t.Fatal(err)
				}
				g := newCatalogPublicationGate(t)
				// Unblock before the catalog helper cleanup waits for active IO.
				t.Cleanup(g.unblock)
				failure := errors.New("fixture publication outcome uncertain")
				if outcome == "post_publication_error" {
					g.postError = failure
				}
				c.publish = g.publish
				ctx, cancelWrite := context.WithCancel(t.Context())
				defer cancelWrite()
				written := make(chan error, 1)
				go func() { written <- fileMutation(c, ctx, sid, kind, "pending") }()
				waitPublication(t, g)
				c.mu.Lock()
				active, done := c.activeFileWrites, c.fileWritesDone
				c.mu.Unlock()
				if active != 1 || done == nil {
					t.Fatalf("admitted file writes=%d done=%v", active, done)
				}
				select {
				case <-done:
					t.Fatal("file completion signalled before publication exit")
				default:
				}
				if a, p := g.counts(); a != 1 || p != 0 {
					t.Fatalf("blocked attempts/completed=%d/%d", a, p)
				}
				if kind == "attachment" {
					aid := c.attachmentID(sid, "pending")
					for _, ext := range []string{".bin", ".json"} {
						if _, err := os.Lstat(filepath.Join(dir, attachmentsDir, aid+ext)); !os.IsNotExist(err) {
							t.Fatalf("file %s published before gate: %v", ext, err)
						}
					}
				}
				closeCtx, cancelClose := context.WithTimeout(t.Context(), 100*time.Millisecond)
				err = c.Close(closeCtx)
				cancelClose()
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("blocked publication Close=%v, want deadline", err)
				}
				c.mu.Lock()
				active, sameDone, writers := c.activeFileWrites, c.fileWritesDone == done, len(c.writers)
				c.mu.Unlock()
				if active != 1 || !sameDone || writers != 0 {
					t.Errorf("timed-out active/same-done/writers=%d/%v/%d", active, sameDone, writers)
				}
				if other, err := NewCatalog(opts); err == nil {
					_ = other.Close(t.Context())
					t.Error("new owner acquired registry before publication exit")
				}
				if err := fileMutation(c, t.Context(), sid, kind, "after-close"); err == nil {
					t.Error("closing catalog accepted new file mutation")
				} else {
					wantCode(t, err, product.CodeStateConflict)
				}
				if a, p := g.counts(); a != 1 || p != 0 {
					t.Errorf("closing intent added publication: %d/%d", a, p)
				}
				closing := make(chan error, 1)
				go func() { closing <- c.Close(context.Background()) }()
				secondReturned := false
				select {
				case err := <-closing:
					secondReturned = true
					t.Errorf("second Close completed before publication exit: %v", err)
				case <-time.After(50 * time.Millisecond):
				}
				if outcome == "cancelled_context" {
					cancelWrite()
				}
				g.unblock()
				writeErr := <-written
				if outcome == "post_publication_error" {
					if !errors.Is(writeErr, failure) {
						t.Errorf("publication error lost: %v", writeErr)
					}
				} else if outcome == "cancelled_context" && kind == "attachment" {
					if !errors.Is(writeErr, context.Canceled) {
						t.Errorf("cancelled attachment=%v", writeErr)
					}
				} else if writeErr != nil {
					t.Errorf("admitted publication=%v", writeErr)
				}
				if !secondReturned {
					select {
					case err := <-closing:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("Close did not finish after real publication exit")
					}
				}
				c.mu.Lock()
				active = c.activeFileWrites
				c.mu.Unlock()
				if active != 0 {
					t.Errorf("file write count leaked after exit: %d", active)
				}
				select {
				case <-done:
				default:
					t.Error("file completion missing after real exit")
				}
				wantPublications := 1
				if kind == "attachment" && outcome == "success" {
					wantPublications = 2
				}
				if a, p := g.counts(); a != wantPublications || p != wantPublications {
					t.Errorf("final attempts/completed=%d/%d, want %d", a, p, wantPublications)
				}
				g.mu.Lock()
				badRecordOrder := g.badRecordOrder
				g.mu.Unlock()
				if badRecordOrder {
					t.Error("attachment record publication preceded actual bytes publication")
				}
				after, err := os.ReadFile(journal)
				if err != nil || !bytes.Equal(before, after) || opts.Model.(*testkit.FakeModel).Calls() != 0 {
					t.Error("file lifecycle changed journal or invoked model")
				}
				other, err := NewCatalog(opts)
				if err != nil {
					t.Fatal("registry retained after file IO actually exited")
				}
				defer other.Close(context.Background())
				if err := fileMutation(c, t.Context(), sid, kind, "old-owner"); err == nil {
					t.Error("closed old owner wrote again")
				} else {
					wantCode(t, err, product.CodeStateConflict)
				}
				if kind == "metadata" {
					got, err := other.GetMetadata(t.Context(), sid)
					if err != nil || got.Name != "pending" || got.Revision != 3 || len(got.Labels) != 1 || got.Labels[0] != "x" {
						t.Errorf("actual metadata=%+v err=%v", got, err)
					}
					prior, duplicate, err := other.SetMetadata(t.Context(), sid, SetMetadataRequest{IdempotencyKey: "original", Name: "one"})
					if err != nil || !duplicate || prior.Revision != 1 || prior.Name != "one" || len(prior.Labels) != 0 {
						t.Errorf("immutable metadata receipt=%+v duplicate=%v err=%v", prior, duplicate, err)
					}
				} else {
					aid := c.attachmentID(sid, "pending")
					data, err := os.ReadFile(filepath.Join(dir, attachmentsDir, aid+".bin"))
					if err != nil || string(data) != "complete bytes" {
						t.Error("real bytes were not published")
					}
					_, _, readErr := other.ReadAttachment(t.Context(), sid, aid)
					if outcome == "success" {
						if readErr != nil {
							t.Fatal(readErr)
						}
						_, duplicate, err := other.SaveAttachment(t.Context(), sid, SaveAttachmentRequest{IdempotencyKey: "pending", MimeType: "text/plain", Name: "pending.txt", Content: []byte("complete bytes")})
						if err != nil || !duplicate {
							t.Error("attachment key receipt lost")
						}
						wantCode(t, fileMutation(other, t.Context(), sid, kind, "sixty-five"), product.CodeBudgetExhausted)
					} else {
						wantCode(t, readErr, product.CodeNotFound)
						if _, err := os.Lstat(filepath.Join(dir, attachmentsDir, aid+".json")); !os.IsNotExist(err) {
							t.Error("failed/cancelled bytes publication exposed a record")
						}
					}
				}
			})
		}
	}
}

func TestCatalogCloseJoinsCleanupAndFileWriteDeadline(t *testing.T) {
	c, opts, sid := attachmentCatalog(t)
	cleanupErr := errors.New("fixture cleanup failed with pending file IO")
	exitedCatalogErrorSession(t, c, cleanupErr)
	g := newCatalogPublicationGate(t)
	c.publish = g.publish
	written := make(chan error, 1)
	go func() { written <- fileMutation(c, t.Context(), sid, "metadata", "pending") }()
	waitPublication(t, g)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	err := c.Close(ctx)
	cancel()
	if !errors.Is(err, cleanupErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("close with pending IO=%v, want cleanup and deadline", err)
	}
	if other, err := store.OpenCreationRegistry(opts.StateRoot); err == nil {
		_ = other.Close()
		t.Error("registry released while file write remains active")
	}
	g.unblock()
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a, p := g.counts(); a != 1 || p != 1 {
		t.Errorf("joined error IO publications=%d/%d", a, p)
	}
}

func TestServerWaitRetainsRegistryUntilAdmittedFilePublicationExit(t *testing.T) {
	conf := testConfig(t)
	setProfile(t, conf, codeagent.ProfileMemory)
	testOptions = func(o *codeagent.Options) { o.Model = testkit.NewFake() }
	server, err := Start(t.Context(), conf, nil)
	testOptions = nil
	if err != nil {
		t.Fatal(err)
	}
	g := newCatalogPublicationGate(t)
	t.Cleanup(func() { g.unblock(); server.Close(); _ = server.Wait() })
	c := server.catalog
	if c == nil {
		t.Fatal("server did not retain its actual catalog owner")
	}
	created, err := c.Create(t.Context(), CatalogCreateRequest{IdempotencyKey: "file-wait", Workspace: c.opts.Workspace, ModelRef: DefaultModelRef})
	if err != nil {
		t.Fatal(err)
	}
	c.publish = g.publish
	written := make(chan error, 1)
	go func() {
		written <- fileMutation(c, context.Background(), created.Snapshot.SessionID, "metadata", "pending")
	}()
	waitPublication(t, g)
	server.Close()
	waited := make(chan error, 1)
	go func() { waited <- server.Wait() }()
	returned := false
	var waitErr error
	select {
	case waitErr = <-waited:
		returned = true
		t.Error("Server.Wait completed before its admitted publication exited")
	case <-time.After(config.WebShutdownTimeout + 250*time.Millisecond):
	}
	if other, err := store.OpenCreationRegistry(c.opts.StateRoot); err == nil {
		_ = other.Close()
		t.Error("Server released registry before file IO exit")
	}
	g.unblock()
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if !returned {
		select {
		case waitErr = <-waited:
		case <-time.After(5 * time.Second):
			t.Fatal("Server.Wait did not finish after real publication exit")
		}
	}
	wantCode(t, waitErr, product.CodeResourceUnavailable)
	if a, p := g.counts(); a != 1 || p != 1 {
		t.Errorf("server attempts/completed=%d/%d", a, p)
	}
	other, err := store.OpenCreationRegistry(c.opts.StateRoot)
	if err != nil {
		t.Fatal("Server retained registry after IO exit")
	}
	_ = other.Close()
}
