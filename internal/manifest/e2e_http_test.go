package manifest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs"
	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
	"github.com/container-registry/helm-charts-oci-proxy/internal/registry"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"helm.sh/helm/v3/pkg/chart"
)

func TestRegistryE2E_ManifestTagsAndRewriteIsolation(t *testing.T) {
	chartV1, err := createTestTarball("demo", &chart.Metadata{
		APIVersion: "v2",
		Name:       "demo",
		Version:    "1.0.0",
		Dependencies: []*chart.Dependency{
			{
				Name:       "redis",
				Version:    "17.0.0",
				Repository: "https://charts.bitnami.com/bitnami",
			},
		},
	})
	if err != nil {
		t.Fatalf("create v1 chart: %v", err)
	}

	chartV2, err := createTestTarball("demo", &chart.Metadata{
		APIVersion: "v2",
		Name:       "demo",
		Version:    "2.0.0",
	})
	if err != nil {
		t.Fatalf("create v2 chart: %v", err)
	}

	indexYAML := []byte(`apiVersion: v1
entries:
  demo:
    - name: demo
      version: v2.0.0
      urls:
        - demo-2.0.0.tgz
    - name: demo
      version: v1.0.0
      urls:
        - demo-1.0.0.tgz
`)

	upstream := newUpstreamChartRepo(t, indexYAML, map[string][]byte{
		"/demo-1.0.0.tgz": chartV1,
		"/demo-2.0.0.tgz": chartV2,
	}, nil)
	defer upstream.Close()

	proxy, _ := newProxyServer(t, upstream.Server.Client(), Config{
		CacheTTL:        time.Minute,
		DownloadTimeout: 5 * time.Second,
		MaxIndexBytes:   1 << 20,
		MaxChartBytes:   1 << 20,
		ProxyHost:       "proxy.example.com",
	})
	defer proxy.Close()

	repoPath := upstream.Host() + "/demo"

	tagsURL := fmt.Sprintf("%s/v2/%s/tags/list?n=1", proxy.URL, repoPath)
	tags := fetchTags(t, proxy.Client(), tagsURL)
	if len(tags.Tags) != 1 || tags.Tags[0] != "1.0.0" {
		t.Fatalf("unexpected paginated tags response: %+v", tags)
	}

	emptyTagsURL := fmt.Sprintf("%s/v2/%s/tags/list?last=2.0.0", proxy.URL, repoPath)
	emptyTags := fetchTags(t, proxy.Client(), emptyTagsURL)
	if len(emptyTags.Tags) != 0 {
		t.Fatalf("expected empty tags after last=2.0.0, got %+v", emptyTags)
	}

	defaultManifestURL := fmt.Sprintf("%s/v2/%s/manifests/1.0.0", proxy.URL, repoPath)
	defaultManifest := fetchManifest(t, proxy.Client(), defaultManifestURL)
	defaultChartBlob := fetchBlob(t, proxy.Client(), proxy.URL, repoPath, defaultManifest.Layers[0].Digest.String())
	defaultMeta, _, err := extractChartYAML(defaultChartBlob)
	if err != nil {
		t.Fatalf("extract default chart yaml: %v", err)
	}
	if got := defaultMeta.Dependencies[0].Repository; got != "https://charts.bitnami.com/bitnami" {
		t.Fatalf("default dependency was unexpectedly rewritten: %s", got)
	}

	rewrittenManifestURL := defaultManifestURL + "?rewrite_dependencies=true"
	rewrittenManifest := fetchManifest(t, proxy.Client(), rewrittenManifestURL)
	rewrittenChartBlob := fetchBlob(t, proxy.Client(), proxy.URL, repoPath, rewrittenManifest.Layers[0].Digest.String())
	rewrittenMeta, _, err := extractChartYAML(rewrittenChartBlob)
	if err != nil {
		t.Fatalf("extract rewritten chart yaml: %v", err)
	}
	if got := rewrittenMeta.Dependencies[0].Repository; got != "oci://proxy.example.com/charts.bitnami.com/bitnami" {
		t.Fatalf("expected rewritten dependency, got %s", got)
	}
	if defaultManifest.Layers[0].Digest == rewrittenManifest.Layers[0].Digest {
		t.Fatal("expected rewritten chart layer digest to differ from default layer digest")
	}

	defaultManifestAgain := fetchManifest(t, proxy.Client(), defaultManifestURL)
	if defaultManifestAgain.Layers[0].Digest != defaultManifest.Layers[0].Digest {
		t.Fatal("default manifest changed after rewritten request")
	}

	if got := upstream.RequestsFor("/demo-1.0.0.tgz"); got != 2 {
		t.Fatalf("expected one upstream fetch per variant, got %d", got)
	}
}

func TestRegistryE2E_ConcurrentRequestsDeduplicateSingleManifestBuild(t *testing.T) {
	chartData, err := createTestTarball("demo", &chart.Metadata{
		APIVersion: "v2",
		Name:       "demo",
		Version:    "1.0.0",
	})
	if err != nil {
		t.Fatalf("create chart: %v", err)
	}

	indexYAML := []byte(`apiVersion: v1
entries:
  demo:
    - name: demo
      version: v1.0.0
      urls:
        - demo-1.0.0.tgz
`)

	upstream := newUpstreamChartRepo(t, indexYAML, map[string][]byte{
		"/demo-1.0.0.tgz": chartData,
	}, map[string]time.Duration{
		"/demo-1.0.0.tgz": 200 * time.Millisecond,
	})
	defer upstream.Close()

	proxy, _ := newProxyServer(t, upstream.Server.Client(), Config{
		CacheTTL:        time.Minute,
		DownloadTimeout: 5 * time.Second,
		MaxIndexBytes:   1 << 20,
		MaxChartBytes:   1 << 20,
	})
	defer proxy.Close()

	manifestURL := fmt.Sprintf("%s/v2/%s/demo/manifests/1.0.0", proxy.URL, upstream.Host())

	const parallel = 8
	var wg sync.WaitGroup
	errCh := make(chan error, parallel)
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := proxy.Client().Get(manifestURL)
			if err != nil {
				errCh <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				errCh <- fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
				return
			}
			_, err = io.ReadAll(resp.Body)
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent request failed: %v", err)
		}
	}

	if got := upstream.RequestsFor("/demo-1.0.0.tgz"); got != 1 {
		t.Fatalf("expected a single upstream chart download, got %d", got)
	}
}

func TestRegistryE2E_DifferentChartsPrepareInParallel(t *testing.T) {
	chartA, err := createTestTarball("alpha", &chart.Metadata{APIVersion: "v2", Name: "alpha", Version: "1.0.0"})
	if err != nil {
		t.Fatalf("create alpha chart: %v", err)
	}
	chartB, err := createTestTarball("bravo", &chart.Metadata{APIVersion: "v2", Name: "bravo", Version: "1.0.0"})
	if err != nil {
		t.Fatalf("create bravo chart: %v", err)
	}

	indexYAML := []byte(`apiVersion: v1
entries:
  alpha:
    - name: alpha
      version: v1.0.0
      urls:
        - alpha-1.0.0.tgz
  bravo:
    - name: bravo
      version: v1.0.0
      urls:
        - bravo-1.0.0.tgz
`)

	upstream := newUpstreamChartRepo(t, indexYAML, map[string][]byte{
		"/alpha-1.0.0.tgz": chartA,
		"/bravo-1.0.0.tgz": chartB,
	}, map[string]time.Duration{
		"/alpha-1.0.0.tgz": 300 * time.Millisecond,
		"/bravo-1.0.0.tgz": 300 * time.Millisecond,
	})
	defer upstream.Close()

	proxy, _ := newProxyServer(t, upstream.Server.Client(), Config{
		CacheTTL:        time.Minute,
		DownloadTimeout: 5 * time.Second,
		MaxIndexBytes:   1 << 20,
		MaxChartBytes:   1 << 20,
	})
	defer proxy.Close()

	urls := []string{
		fmt.Sprintf("%s/v2/%s/alpha/manifests/1.0.0", proxy.URL, upstream.Host()),
		fmt.Sprintf("%s/v2/%s/bravo/manifests/1.0.0", proxy.URL, upstream.Host()),
	}

	start := time.Now()
	var wg sync.WaitGroup
	errCh := make(chan error, len(urls))
	for _, manifestURL := range urls {
		wg.Add(1)
		go func(manifestURL string) {
			defer wg.Done()
			resp, err := proxy.Client().Get(manifestURL)
			if err != nil {
				errCh <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				errCh <- fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
				return
			}
			_, err = io.ReadAll(resp.Body)
			errCh <- err
		}(manifestURL)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("parallel manifest request failed: %v", err)
		}
	}

	elapsed := time.Since(start)
	if elapsed >= 550*time.Millisecond {
		t.Fatalf("expected chart preparations to overlap, took %v", elapsed)
	}
	if upstream.RequestsFor("/alpha-1.0.0.tgz") != 1 || upstream.RequestsFor("/bravo-1.0.0.tgz") != 1 {
		t.Fatalf("expected one fetch per chart, got alpha=%d bravo=%d", upstream.RequestsFor("/alpha-1.0.0.tgz"), upstream.RequestsFor("/bravo-1.0.0.tgz"))
	}
}

func TestRegistryE2E_InvalidTagAndCatalogQueriesReturnBadRequest(t *testing.T) {
	proxy, _ := newProxyServer(t, &http.Client{Timeout: 5 * time.Second}, Config{
		CacheTTL:        time.Minute,
		DownloadTimeout: 5 * time.Second,
		MaxIndexBytes:   1 << 20,
		MaxChartBytes:   1 << 20,
	})
	defer proxy.Close()

	tests := []struct {
		name string
		url  string
	}{
		{
			name: "negative tags n",
			url:  proxy.URL + "/v2/example.com/demo/tags/list?n=-1",
		},
		{
			name: "missing tags repository",
			url:  proxy.URL + "/x/v2/tags/list",
		},
		{
			name: "negative catalog n",
			url:  proxy.URL + "/v2/_catalog?n=-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := proxy.Client().Get(tt.url)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.url, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected 400, got %d: %s", resp.StatusCode, string(body))
			}
		})
	}
}

func newProxyServer(t *testing.T, upstreamClient *http.Client, cfg Config) (*httptest.Server, *Manifests) {
	t.Helper()
	ctx := context.Background()
	blobsHandler := mem.NewMemHandler()
	manifests := NewManifests(ctx, blobsHandler, cfg, &mockCache{}, &mockLogger{})
	manifests.httpClient = upstreamClient

	h := registry.New(
		manifests.Handle,
		blobs.NewBlobs(blobsHandler, &mockLogger{}).Handle,
		manifests.HandleTags,
		manifests.HandleCatalog,
		registry.Logger(&mockLogger{}),
	)

	return httptest.NewServer(h), manifests
}

type upstreamChartRepo struct {
	Server   *httptest.Server
	requests sync.Map
	delays   map[string]time.Duration
	indexYML []byte
	charts   map[string][]byte
}

func newUpstreamChartRepo(t *testing.T, indexYAML []byte, charts map[string][]byte, delays map[string]time.Duration) *upstreamChartRepo {
	t.Helper()
	repo := &upstreamChartRepo{
		delays:   delays,
		indexYML: indexYAML,
		charts:   charts,
	}
	repo.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo.increment(r.URL.Path)
		if delay := repo.delays[r.URL.Path]; delay > 0 {
			time.Sleep(delay)
		}
		switch r.URL.Path {
		case "/index.yaml":
			w.Header().Set("Content-Type", "application/x-yaml")
			_, _ = w.Write(repo.indexYML)
		default:
			chartData, ok := repo.charts[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(chartData)
		}
	}))
	return repo
}

func (r *upstreamChartRepo) Close() {
	r.Server.Close()
}

func (r *upstreamChartRepo) Host() string {
	return strings.TrimPrefix(r.Server.URL, "https://")
}

func (r *upstreamChartRepo) RequestsFor(path string) int32 {
	v, ok := r.requests.Load(path)
	if !ok {
		return 0
	}
	return v.(*atomic.Int32).Load()
}

func (r *upstreamChartRepo) increment(path string) {
	v, _ := r.requests.LoadOrStore(path, &atomic.Int32{})
	v.(*atomic.Int32).Add(1)
}

func fetchManifest(t *testing.T, client *http.Client, manifestURL string) ocispec.Manifest {
	t.Helper()
	resp, err := client.Get(manifestURL)
	if err != nil {
		t.Fatalf("get manifest %s: %v", manifestURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("manifest request failed: %d %s", resp.StatusCode, string(body))
	}
	var manifest ocispec.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest.Layers) == 0 {
		t.Fatal("manifest contained no layers")
	}
	return manifest
}

func fetchBlob(t *testing.T, client *http.Client, proxyURL string, repoPath string, digest string) []byte {
	t.Helper()
	blobURL := fmt.Sprintf("%s/v2/%s/blobs/%s", proxyURL, repoPath, digest)
	resp, err := client.Get(blobURL)
	if err != nil {
		t.Fatalf("get blob %s: %v", blobURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("blob request failed: %d %s", resp.StatusCode, string(body))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read blob %s: %v", blobURL, err)
	}
	return data
}

func fetchTags(t *testing.T, client *http.Client, tagsURL string) listTags {
	t.Helper()
	resp, err := client.Get(tagsURL)
	if err != nil {
		t.Fatalf("get tags %s: %v", tagsURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("tags request failed: %d %s", resp.StatusCode, string(body))
	}
	var tags listTags
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		t.Fatalf("decode tags response: %v", err)
	}
	return tags
}
