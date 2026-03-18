package manifest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	blobhandler "github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestValidateProxyHost(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		want    string
		wantErr bool
	}{
		{name: "plain host", host: "chartproxy.example.com", want: "chartproxy.example.com"},
		{name: "host and port", host: "chartproxy.example.com:5000", want: "chartproxy.example.com:5000"},
		{name: "empty", host: "", wantErr: true},
		{name: "with scheme", host: "https://chartproxy.example.com", wantErr: true},
		{name: "with path", host: "chartproxy.example.com/path", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateProxyHost(tt.host)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateProxyHost() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("validateProxyHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetRewriteOptionsRequiresConfiguredProxyHost(t *testing.T) {
	m := &Manifests{config: Config{RewriteDependencies: true}}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/v2/repo/manifests/1.0.0", nil)

	_, regErr := m.getRewriteOptions(req)
	if regErr == nil {
		t.Fatal("expected error when rewrite is enabled without PROXY_HOST")
	}
}

func TestParseIndexFileFiltersInvalidVersions(t *testing.T) {
	indexYAML := []byte(`apiVersion: v1
entries:
  demo:
    - name: demo
      version: 1.0.0
      urls:
        - https://example.com/demo-1.0.0.tgz
    - name: demo
      version: definitely-not-semver
      urls:
        - https://example.com/demo-invalid.tgz
`)

	idx, err := parseIndexFile(indexYAML)
	if err != nil {
		t.Fatalf("parseIndexFile() error = %v", err)
	}
	versions := idx.Entries["demo"]
	if len(versions) != 1 {
		t.Fatalf("expected 1 valid version, got %d", len(versions))
	}
	if versions[0].Version != "1.0.0" {
		t.Fatalf("expected remaining version 1.0.0, got %q", versions[0].Version)
	}
}

func TestDownloadValidatesStatusAndSize(t *testing.T) {
	statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer statusServer.Close()

	m := &Manifests{httpClient: statusServer.Client(), log: &mockLogger{}}
	if _, err := m.download(context.Background(), statusServer.URL, 1024); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected non-200 download error, got %v", err)
	}

	largeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("a", 8))
	}))
	defer largeServer.Close()

	m.httpClient = largeServer.Client()
	if _, err := m.download(context.Background(), largeServer.URL, 4); err == nil || !strings.Contains(err.Error(), "exceeded max size") {
		t.Fatalf("expected size limit error, got %v", err)
	}
}

func TestCleanupExpiredPreservesSharedRefs(t *testing.T) {
	ref := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	unique := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	h := &deleteTrackingBlobHandler{}
	m := &Manifests{
		blobHandler: h,
		config: Config{CacheTTL: time.Minute},
		log:    &mockLogger{},
		manifests: map[string]map[string]map[string]Manifest{
			"repo": {
				defaultVariantKey: {
					"old": {Refs: []string{ref, unique}, CreatedAt: time.Now().Add(-2 * time.Minute)},
					"new": {Refs: []string{ref}, CreatedAt: time.Now()},
				},
			},
		},
	}

	m.cleanupExpired(context.Background())

	if len(h.deleted) != 1 || h.deleted[0] != unique {
		t.Fatalf("expected only unique ref to be deleted, got %v", h.deleted)
	}
}

type deleteTrackingBlobHandler struct {
	deleted []string
}

var _ blobhandler.BlobDeleteHandler = (*deleteTrackingBlobHandler)(nil)

func (h *deleteTrackingBlobHandler) Get(ctx context.Context, repo string, hash v1.Hash) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (h *deleteTrackingBlobHandler) Delete(ctx context.Context, repo string, hash v1.Hash) error {
	h.deleted = append(h.deleted, hash.String())
	return nil
}

func TestVariantKeyIsolation(t *testing.T) {
	m := &Manifests{}
	plain := m.variantKey(RewriteOptions{})
	rewritten := m.variantKey(RewriteOptions{Enabled: true, ProxyHost: "chartproxy.example.com"})

	if plain == rewritten {
		t.Fatalf("expected variant keys to differ, both were %q", plain)
	}
	if rewritten != fmt.Sprintf("rewrite:%s", "chartproxy.example.com") {
		t.Fatalf("unexpected rewrite variant key %q", rewritten)
	}
}
