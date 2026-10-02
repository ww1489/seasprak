package web

import (
	"context"
	"strconv"
	"testing"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestMetadataDurableIdempotentAndBounded(t *testing.T) {
	c, opts, sid := attachmentCatalog(t)
	m, dup, err := c.SetMetadata(t.Context(), sid, SetMetadataRequest{IdempotencyKey: "a", Name: "one", Labels: []string{"x"}})
	if err != nil || dup || m.Revision != 1 {
		t.Fatalf("set: %v %+v", err, m)
	}
	if _, dup, err = c.SetMetadata(t.Context(), sid, SetMetadataRequest{IdempotencyKey: "a", Name: "one", Labels: []string{"x"}}); err != nil || !dup {
		t.Fatal("replay not recognised")
	}
	_, _, err = c.SetMetadata(t.Context(), sid, SetMetadataRequest{IdempotencyKey: "a", Name: "two"})
	wantCode(t, err, product.CodeIdempotencyConflict)
	for i := 0; i < config.SessionMetadataKeysKept; i++ {
		if _, _, err = c.SetMetadata(t.Context(), sid, SetMetadataRequest{IdempotencyKey: "k" + strconv.Itoa(i), Name: "n"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	c, err = NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	got, err := c.GetMetadata(t.Context(), sid)
	if err != nil || got.Name != "n" || got.Revision != uint64(1+config.SessionMetadataKeysKept) {
		t.Fatalf("reopened metadata %+v %v", got, err)
	}
	last := "k" + strconv.Itoa(config.SessionMetadataKeysKept-1)
	if _, dup, err = c.SetMetadata(t.Context(), sid, SetMetadataRequest{IdempotencyKey: last, Name: "n"}); err != nil || !dup {
		t.Fatal("durable key lost across reopen")
	}
	for _, bad := range []SetMetadataRequest{{IdempotencyKey: "b", Name: " padded"}, {IdempotencyKey: "b", Labels: []string{""}}, {IdempotencyKey: "b", Labels: make([]string, config.SessionLabels+1)}, {Name: "no key"}} {
		_, _, err = c.SetMetadata(t.Context(), sid, bad)
		wantCode(t, err, product.CodeInvalidArgument)
	}
	_, _, err = c.SetMetadata(t.Context(), "missing", SetMetadataRequest{IdempotencyKey: "b"})
	wantCode(t, err, product.CodeNotFound)
}
