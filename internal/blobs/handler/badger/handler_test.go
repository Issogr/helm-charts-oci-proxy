package badger

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dgraph-io/badger/v3"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestGetReturnsReadableCopy(t *testing.T) {
	dir := t.TempDir()
	db, err := badger.Open(badger.DefaultOptions(filepath.Join(dir, "badger")).WithLogger(nil))
	if err != nil {
		t.Fatalf("opening badger: %v", err)
	}
	defer func() {
		_ = db.Close()
	}()

	hash, err := v1.NewHash("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("creating hash: %v", err)
	}

	h := NewHandler(db)
	if err := h.Put(context.Background(), "", hash, io.NopCloser(strings.NewReader("hello world"))); err != nil {
		t.Fatalf("put: %v", err)
	}

	rc, err := h.Get(context.Background(), "", hash)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("unexpected data %q", string(data))
	}

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("temp dir should still exist: %v", err)
	}
}
