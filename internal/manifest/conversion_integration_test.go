//go:build integration

package manifest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/chart"
	"sigs.k8s.io/yaml"
)

func TestConversion_Integration_GrafanaAlloyMatchesOriginalArchive(t *testing.T) {
	const (
		indexURL  = "https://grafana.github.io/helm-charts/index.yaml"
		chartName = "alloy"
		version   = "1.6.2"
	)

	originalChartURL := resolveChartURL(t, indexURL, chartName, version)
	originalChart := downloadBytes(t, originalChartURL)

	proxy, _ := newProxyServer(t, &http.Client{Timeout: 30 * time.Second}, Config{
		CacheTTL:        time.Minute,
		DownloadTimeout: 30 * time.Second,
		MaxIndexBytes:   32 << 20,
		MaxChartBytes:   256 << 20,
	})
	defer proxy.Close()

	repoPath := "grafana.github.io/helm-charts/alloy"
	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", proxy.URL, repoPath, version)
	manifest := fetchManifest(t, proxy.Client(), manifestURL)
	convertedChart := fetchBlob(t, proxy.Client(), proxy.URL, repoPath, manifest.Layers[0].Digest.String())

	if !bytes.Equal(convertedChart, originalChart) {
		t.Fatal("converted OCI chart layer differs from the original online Grafana Alloy chart archive")
	}
}

func TestConversion_Integration_GrafanaAlloyOperatorRewriteOnlyChangesDependencyProtocol(t *testing.T) {
	const (
		indexURL  = "https://grafana.github.io/helm-charts/index.yaml"
		chartName = "alloy-operator"
		version   = "0.5.2"
		proxyHost = "proxy.example.com"
		repoPath  = "grafana.github.io/helm-charts/alloy-operator"
	)

	originalChartURL := resolveChartURL(t, indexURL, chartName, version)
	originalChart := downloadBytes(t, originalChartURL)

	proxy, _ := newProxyServer(t, &http.Client{Timeout: 30 * time.Second}, Config{
		CacheTTL:        time.Minute,
		DownloadTimeout: 30 * time.Second,
		MaxIndexBytes:   32 << 20,
		MaxChartBytes:   256 << 20,
		ProxyHost:       proxyHost,
	})
	defer proxy.Close()

	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s?rewrite_dependencies=true", proxy.URL, repoPath, version)
	manifest := fetchManifest(t, proxy.Client(), manifestURL)
	rewrittenChart := fetchBlob(t, proxy.Client(), proxy.URL, repoPath, manifest.Layers[0].Digest.String())

	originalFiles, err := extractTarballFiles(originalChart)
	if err != nil {
		t.Fatalf("extract original chart files: %v", err)
	}
	rewrittenFiles, err := extractTarballFiles(rewrittenChart)
	if err != nil {
		t.Fatalf("extract rewritten chart files: %v", err)
	}

	if !reflect.DeepEqual(sortedKeys(originalFiles), sortedKeys(rewrittenFiles)) {
		t.Fatalf("rewritten chart changed file set: original=%v rewritten=%v", sortedKeys(originalFiles), sortedKeys(rewrittenFiles))
	}

	for path, originalContent := range originalFiles {
		if strings.HasSuffix(path, "/Chart.yaml") {
			continue
		}
		rewrittenContent, ok := rewrittenFiles[path]
		if !ok {
			t.Fatalf("rewritten chart missing file %s", path)
		}
		if !bytes.Equal(rewrittenContent, originalContent) {
			t.Fatalf("file %s changed during rewrite", path)
		}
	}

	originalMeta, originalChartPath, err := extractChartYAML(originalChart)
	if err != nil {
		t.Fatalf("extract original Chart.yaml: %v", err)
	}
	rewrittenMeta, rewrittenChartPath, err := extractChartYAML(rewrittenChart)
	if err != nil {
		t.Fatalf("extract rewritten Chart.yaml: %v", err)
	}
	if originalChartPath != rewrittenChartPath {
		t.Fatalf("Chart.yaml path changed: original=%s rewritten=%s", originalChartPath, rewrittenChartPath)
	}

	expectedMeta := cloneMetadata(t, originalMeta)
	for _, dep := range expectedMeta.Dependencies {
		if !shouldRewriteURL(dep.Repository) {
			continue
		}
		newURL, _, err := rewriteDependencyURL(dep.Repository, proxyHost)
		if err != nil {
			t.Fatalf("rewrite dependency %s: %v", dep.Name, err)
		}
		dep.Repository = newURL
	}

	if !reflect.DeepEqual(expectedMeta, rewrittenMeta) {
		expectedYAML, _ := yaml.Marshal(expectedMeta)
		gotYAML, _ := yaml.Marshal(rewrittenMeta)
		t.Fatalf("rewritten Chart.yaml differs beyond dependency protocol rewrite\nexpected:\n%s\ngot:\n%s", string(expectedYAML), string(gotYAML))
	}
}

func extractTarballFiles(data []byte) (map[string][]byte, error) {
	gzr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[hdr.Name] = content
	}
}

func sortedKeys[K ~string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func cloneMetadata(t *testing.T, metadata *chart.Metadata) *chart.Metadata {
	t.Helper()
	raw, err := yaml.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	var cloned chart.Metadata
	if err := yaml.Unmarshal(raw, &cloned); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	return &cloned
}
