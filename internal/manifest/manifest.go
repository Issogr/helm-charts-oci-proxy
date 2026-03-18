package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler"
	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
	"github.com/container-registry/helm-charts-oci-proxy/internal/helper"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Catalog struct {
	Repos []string `json:"repositories"`
}

type listTags struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

type Manifest struct {
	ContentType string    `json:"contentType"`
	Blob        []byte    `json:"blob"`
	Refs        []string  `json:"refs"` // referenced blobs digests
	CreatedAt   time.Time `json:"createdAt"`
}

type Manifests struct {
	// maps repo -> variant -> Manifest tag/digest -> Manifest
	manifests   map[string]map[string]map[string]Manifest
	lock        sync.RWMutex
	prepare     singleflight.Group
	log         logrus.StdLogger
	cache       Cache
	blobHandler handler.BlobHandler
	config      Config
	httpClient  *http.Client
	baseContext context.Context
}

func NewManifests(ctx context.Context, blobHandler handler.BlobHandler, config Config, cache Cache, log logrus.StdLogger) *Manifests {
	if config.DownloadTimeout <= 0 {
		config.DownloadTimeout = 30 * time.Second
	}

	ma := &Manifests{

		manifests:   map[string]map[string]map[string]Manifest{},
		blobHandler: blobHandler,
		log:         log,
		config:      config,
		cache:       cache,
		httpClient: &http.Client{
			Timeout: config.DownloadTimeout,
		},
		baseContext: ctx,
	}

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if ma.config.Debug {
					ma.log.Println("cleanup cycle")
				}
				ma.cleanupExpired(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()

	return ma
}

// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pulling-an-image-manifest
// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pushing-an-image
func (m *Manifests) Handle(resp http.ResponseWriter, req *http.Request) error {
	elem := strings.Split(req.URL.Path, "/")

	if len(elem) < 3 {
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "INVALID PARAMS",
			Message: "No chart name specified",
		}
	}

	elem = elem[1:]
	target := elem[len(elem)-1]
	if target != "" && strings.HasPrefix(target, "v") {
		target = target[1:]
	}

	var repoParts []string
	for i := len(elem) - 3; i > 0; i-- {
		if elem[i] == "v2" {
			//enough
			break
		}
		repoParts = append(repoParts, elem[i])
	}
	sort.SliceStable(repoParts, func(i, j int) bool {
		//reverse
		return i > j
	})
	repo := strings.Join(repoParts, "/")

	// Determine rewrite options from config and query params
	rewriteOpts, regErr := m.getRewriteOptions(req)
	if regErr != nil {
		return regErr
	}

	switch req.Method {
	case http.MethodGet:
		ma, err := m.getOrPrepareManifest(req.Context(), repo, target, rewriteOpts)
		if err != nil {
			return err
		}
		rd := sha256.Sum256(ma.Blob)
		d := "sha256:" + hex.EncodeToString(rd[:])
		resp.Header().Set("Docker-Content-Digest", d)
		resp.Header().Set("Content-Type", ma.ContentType)
		resp.Header().Set("Content-Length", fmt.Sprint(len(ma.Blob)))
		resp.WriteHeader(http.StatusOK)
		_, err = io.Copy(resp, bytes.NewReader(ma.Blob))
		if err != nil {
			return errors.RegErrInternal(err)
		}
		return nil

	case http.MethodHead:
		ma, err := m.getOrPrepareManifest(req.Context(), repo, target, rewriteOpts)
		if err != nil {
			return err
		}
		rd := sha256.Sum256(ma.Blob)
		d := "sha256:" + hex.EncodeToString(rd[:])
		resp.Header().Set("Docker-Content-Digest", d)
		resp.Header().Set("Content-Type", ma.ContentType)
		resp.Header().Set("Content-Length", fmt.Sprint(len(ma.Blob)))
		resp.WriteHeader(http.StatusOK)
		return nil

	default:
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}
}

func (m *Manifests) HandleTags(resp http.ResponseWriter, req *http.Request) error {
	elem := strings.Split(req.URL.Path, "/")
	if len(elem) < 4 {
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "INVALID PARAMS",
			Message: "No chart name specified",
		}
	}
	var repoParts []string
	for i := len(elem) - 3; i > 0; i-- {
		if elem[i] == "v2" {
			//stop
			break
		}
		repoParts = append(repoParts, elem[i])
	}
	sort.SliceStable(repoParts, func(i, j int) bool {
		//reverse
		return i > j
	})
	fullRepo := strings.Join(repoParts, "/")

	if req.Method != "GET" {
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}
	// Determine rewrite options from config and query params
	rewriteOpts, regErr := m.getRewriteOptions(req)
	if regErr != nil {
		return regErr
	}

	repoPath := strings.Join(repoParts[:len(repoParts)-1], "/")
	var tags []string

	index, _ := m.GetIndex(req.Context(), repoPath)

	if index != nil {
		if versions, ok := index.Entries[repoParts[len(repoParts)-1]]; ok {
			for _, v := range versions {
				tags = append(tags, strings.TrimLeft(v.Version, "v"))
			}
		}
	} else {
		if err := m.prepareManifest(req.Context(), fullRepo, "", rewriteOpts); err != nil {
			return err
		}
		c := m.getVariant(fullRepo, m.variantKey(rewriteOpts))
		for tag := range c {
			if !strings.Contains(tag, "sha256:") {
				tags = append(tags, tag)
			}
		}
	}
	sort.Strings(tags)

	// https://github.com/opencontainers/distribution-spec/blob/b505e9cc53ec499edbd9c1be32298388921bb705/detail.md#tags-paginated
	// Offset using last query parameter.
	if last := req.URL.Query().Get("last"); last != "" {
		i := sort.SearchStrings(tags, last)
		for i < len(tags) && tags[i] <= last {
			i++
		}
		tags = tags[i:]
	}

	// Limit using n query parameter.
	if ns := req.URL.Query().Get("n"); ns != "" {
		if n, err := strconv.Atoi(ns); err != nil {
			return &errors.RegError{
				Status:  http.StatusBadRequest,
				Code:    "BAD_REQUEST",
				Message: fmt.Sprintf("parsing n: %v", err),
			}
		} else if n < len(tags) {
			tags = tags[:n]
		}
	}

	tagsToList := listTags{
		Name: fullRepo,
		Tags: tags,
	}

	msg, _ := json.Marshal(tagsToList)
	resp.Header().Set("Content-Length", fmt.Sprint(len(msg)))
	resp.WriteHeader(http.StatusOK)
	_, err := io.Copy(resp, bytes.NewReader(msg))
	if err != nil {
		return errors.RegErrInternal(err)
	}
	return nil
}

func (m *Manifests) Read(repo string, name string) (Manifest, error) {
	return m.ReadVariant(repo, defaultVariantKey, name)
}

func (m *Manifests) ReadVariant(repo string, variant string, name string) (Manifest, error) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	variants, ok := m.manifests[repo]
	if !ok {
		return Manifest{}, fmt.Errorf("repository not found")
	}
	mRepo, ok := variants[variant]
	if !ok {
		return Manifest{}, fmt.Errorf("manifest variant not found")
	}
	ma, ok := mRepo[name]
	if !ok {
		return Manifest{}, fmt.Errorf("manifest not found")
	}
	return ma, nil
}

func (m *Manifests) Write(repo string, name string, n Manifest) error {
	return m.WriteVariant(repo, defaultVariantKey, name, n)
}

func (m *Manifests) WriteVariant(repo string, variant string, name string, n Manifest) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	variants, ok := m.manifests[repo]
	if !ok {
		variants = map[string]map[string]Manifest{}
		m.manifests[repo] = variants
	}
	mRepo, ok := variants[variant]
	if !ok {
		mRepo = map[string]Manifest{}
		variants[variant] = mRepo
	}
	mRepo[name] = n
	return nil
}

func (m *Manifests) HandleCatalog(resp http.ResponseWriter, req *http.Request) error {
	query := req.URL.Query()
	nStr := query.Get("n")
	n := 10000
	if nStr != "" {
		var err error
		n, err = strconv.Atoi(nStr)
		if err != nil {
			return errors.RegErrInternal(err)
		}
	}

	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]

	if req.Method != "GET" {
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}

	var repos []string
	countRepos := 0

	if len(elems) > 2 {
		// we have repo
		repo := strings.Join(elems[0:len(elems)-2], "/")
		index, _ := m.GetIndex(req.Context(), repo)
		if index != nil {
			// show index's content instead of local
			for r := range index.Entries {
				if countRepos >= n {
					break
				}
				countRepos++
				repos = append(repos, fmt.Sprintf("%s/%s", repo, r))
			}
		}

	} else {
		m.lock.RLock()
		defer m.lock.RUnlock()

		// TODO: implement pagination
		for key := range m.manifests {
			if countRepos >= n {
				break
			}
			countRepos++
			repos = append(repos, key)
		}
	}

	sort.Strings(repos)
	repositoriesToList := Catalog{
		Repos: repos,
	}

	msg, _ := json.Marshal(repositoriesToList)
	resp.Header().Set("Content-Length", fmt.Sprint(len(msg)))
	resp.WriteHeader(http.StatusOK)
	_, err := io.Copy(resp, bytes.NewReader([]byte(msg)))
	if err != nil {
		return errors.RegErrInternal(err)
	}
	return nil
}

const defaultVariantKey = "default"

// getRewriteOptions determines rewrite options from config and query parameters.
// Query parameter "rewrite_dependencies" overrides the config setting.
func (m *Manifests) getRewriteOptions(req *http.Request) (RewriteOptions, *errors.RegError) {
	// Start with config value
	enabled := m.config.RewriteDependencies

	// Query param overrides config
	if qp := req.URL.Query().Get("rewrite_dependencies"); qp != "" {
		enabled = qp == "true" || qp == "1"
	}

	if !enabled {
		return RewriteOptions{}, nil
	}

	proxyHost, err := validateProxyHost(m.config.ProxyHost)
	if err != nil {
		return RewriteOptions{}, errors.RegErrInternal(err)
	}

	return RewriteOptions{
		Enabled:   enabled,
		ProxyHost: proxyHost,
	}, nil
}

func validateProxyHost(proxyHost string) (string, error) {
	proxyHost = strings.TrimSpace(proxyHost)
	if proxyHost == "" {
		return "", fmt.Errorf("PROXY_HOST must be set when dependency rewriting is enabled")
	}
	parsed, err := url.Parse("//" + proxyHost)
	if err != nil {
		return "", fmt.Errorf("invalid PROXY_HOST: %w", err)
	}
	if parsed.Host == "" || parsed.Path != "" || strings.ContainsAny(proxyHost, "/?#") {
		return "", fmt.Errorf("invalid PROXY_HOST %q", proxyHost)
	}
	return parsed.Host, nil
}

func (m *Manifests) variantKey(opts RewriteOptions) string {
	if !opts.Enabled {
		return defaultVariantKey
	}
	return "rewrite:" + opts.ProxyHost
}

func (m *Manifests) getVariant(repo string, variant string) map[string]Manifest {
	m.lock.RLock()
	defer m.lock.RUnlock()

	variants, ok := m.manifests[repo]
	if !ok {
		return nil
	}
	repoManifests := variants[variant]
	if repoManifests == nil {
		return nil
	}
	copyOfRepo := make(map[string]Manifest, len(repoManifests))
	for k, v := range repoManifests {
		copyOfRepo[k] = v
	}
	return copyOfRepo
}

func (m *Manifests) getOrPrepareManifest(ctx context.Context, repo string, target string, opts RewriteOptions) (Manifest, error) {
	variant := m.variantKey(opts)
	if ma, err := m.ReadVariant(repo, variant, target); err == nil {
		return ma, nil
	}

	if err := m.prepareManifest(ctx, repo, target, opts); err != nil {
		return Manifest{}, err
	}

	if ma, err := m.ReadVariant(repo, variant, target); err == nil {
		return ma, nil
	}

	normalized := helper.SemVerReplace(target)
	if normalized != target {
		if ma, err := m.ReadVariant(repo, variant, normalized); err == nil {
			return ma, nil
		}
	}

	return Manifest{}, &errors.RegError{
		Status:  http.StatusNotFound,
		Code:    "NOT FOUND",
		Message: fmt.Sprintf("Chart prepare's result not found: %v, %v", repo, target),
	}
}

func (m *Manifests) prepareManifest(ctx context.Context, repo string, target string, opts RewriteOptions) error {
	variant := m.variantKey(opts)
	prepareTarget := target
	if prepareTarget == "" {
		prepareTarget = "latest"
	}
	key := repo + "|" + variant + "|" + prepareTarget
	_, err, _ := m.prepare.Do(key, func() (interface{}, error) {
		if target == "" {
			if m.hasVariant(repo, variant) {
				return nil, nil
			}
		} else if _, readErr := m.ReadVariant(repo, variant, target); readErr == nil {
			return nil, nil
		}

		prepareCtx := ctx
		if prepareCtx == nil {
			prepareCtx = m.baseContext
		}
		if prepareCtx == nil {
			prepareCtx = context.Background()
		}
		if m.config.DownloadTimeout > 0 {
			var cancel context.CancelFunc
			prepareCtx, cancel = context.WithTimeout(prepareCtx, m.config.DownloadTimeout)
			defer cancel()
		}
		if target == "" {
			return nil, m.prepareChart(prepareCtx, repo, "", opts)
		}
		err := m.prepareChart(prepareCtx, repo, target, opts)
		if err == nil {
			return nil, nil
		}
		normalized := helper.SemVerReplace(target)
		if normalized != target {
			return nil, m.prepareChart(prepareCtx, repo, normalized, opts)
		}
		return nil, err
	})
	return err
}

func (m *Manifests) hasVariant(repo string, variant string) bool {
	m.lock.RLock()
	defer m.lock.RUnlock()

	variants, ok := m.manifests[repo]
	if !ok {
		return false
	}
	repoManifests, ok := variants[variant]
	return ok && len(repoManifests) > 0
}

func (m *Manifests) cleanupExpired(ctx context.Context) {
	cutoff := time.Now().Add(-m.config.CacheTTL)
	refsInUse := map[string]int{}
	refsToDelete := map[string]struct{}{}

	m.lock.Lock()
	for repo, variants := range m.manifests {
		for variant, manifests := range variants {
			for name, ma := range manifests {
				if ma.CreatedAt.Before(cutoff) {
					delete(manifests, name)
					for _, ref := range ma.Refs {
						refsToDelete[ref] = struct{}{}
					}
					continue
				}
				for _, ref := range ma.Refs {
					refsInUse[ref]++
				}
			}
			if len(manifests) == 0 {
				delete(variants, variant)
			}
		}
		if len(variants) == 0 {
			delete(m.manifests, repo)
		}
	}
	m.lock.Unlock()

	delHandler, ok := m.blobHandler.(handler.BlobDeleteHandler)
	if !ok {
		return
	}
	for ref := range refsToDelete {
		if refsInUse[ref] > 0 {
			continue
		}
		h, err := v1.NewHash(ref)
		if err != nil {
			continue
		}
		if m.config.Debug {
			m.log.Printf("deleting blob %s", h.String())
		}
		if err = delHandler.Delete(ctx, "", h); err != nil {
			m.log.Println(err)
		}
	}
}
