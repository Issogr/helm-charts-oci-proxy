package manifest

import "time"

type Config struct {
	Debug              bool
	CacheTTL           time.Duration // for how long store manifest
	IndexCacheTTL      time.Duration
	IndexErrorCacheTTl time.Duration
	DownloadTimeout    time.Duration
	MaxIndexBytes      int64
	MaxChartBytes      int64

	// RewriteDependencies enables rewriting of chart dependency URLs to point through the proxy
	RewriteDependencies bool
	// ProxyHost is the hostname used for rewritten dependency URLs.
	// It must be explicitly configured when dependency rewriting is enabled.
	ProxyHost string
}
