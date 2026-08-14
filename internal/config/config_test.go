package config_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hcl/domino-mule/internal/config"
)

func TestLoadHoneycombFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mule.yaml")
	content := `
source:
  file_path: "D:/Mule/data/domino_stats.json"
  poll_interval: 60s

exporter:
  endpoint: "https://api.honeycomb.io"
  path: "/v1/metrics"
  headers:
    x-honeycomb-team: "test-key"
  timeout: 10s

metrics:
  prefix: "domino"
  include_patterns:
    - "^Server\\.Users.*"
  include_prefixes:
    - "HTTP."
  exclude_patterns:
    - '(?i)\.name$'
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	wantPath := config.NormalizePath(`D:\Mule\data\domino_stats.json`)
	if cfg.Source.FilePath != wantPath {
		t.Fatalf("file_path: got %q want %q", cfg.Source.FilePath, wantPath)
	}

	url, err := cfg.Exporter.EndpointURL()
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://api.honeycomb.io/v1/metrics" {
		t.Fatalf("url=%q", url)
	}
	if !cfg.Source.PruneEnabled() {
		t.Fatal("prune should default to true when omitted")
	}

	if !cfg.Metrics.Allow("Server.Users") {
		t.Fatal("Server.Users should match include_patterns")
	}
	if !cfg.Metrics.Allow("HTTP.WorkerThreads.Busy") {
		t.Fatal("HTTP.* should match include_prefixes")
	}
	if cfg.Metrics.Allow("Mail.TotalPending") {
		t.Fatal("Mail.TotalPending should be denied")
	}
}

func TestNormalizePathMixedSeparators(t *testing.T) {
	got := config.NormalizePath(`D:\Mule/data/domino_stats.json`)
	want := config.NormalizePath("D:/Mule/data/domino_stats.json")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if runtime.GOOS == "windows" {
		if got != `D:\Mule\data\domino_stats.json` {
			t.Fatalf("windows path: got %q", got)
		}
	}
}

func TestLoadSampleMuleYAML(t *testing.T) {
	cfg, err := config.Load("../../mule.yaml")
	if err != nil {
		t.Fatal(err)
	}
	u, err := cfg.Exporter.EndpointURL()
	if err != nil {
		t.Fatal(err)
	}
	if u != "https://api.honeycomb.io/v1/metrics" {
		t.Fatalf("url=%q", u)
	}
	t.Log("stats_file=", cfg.Source.FilePath)
}

func TestEndpointURLDefaultsPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mule.yaml")
	content := `
source:
  file_path: "/tmp/stats.json"
  poll_interval: 60s
exporter:
  endpoint: "https://api.honeycomb.io"
  headers:
    x-honeycomb-team: "x"
  timeout: 10s
metrics:
  prefix: domino
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	url, err := cfg.Exporter.EndpointURL()
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://api.honeycomb.io/v1/metrics" {
		t.Fatalf("got %q", url)
	}
}

func TestLoadScrapes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mule.yaml")
	content := `
source:
  file_path: "/tmp/stats.json"
  poll_interval: 60s
scrapes:
  - url: "http://localhost:8890/metrics"
    timeout: 5s
  - url: "http://127.0.0.1:9100/metrics"
    source: "node_exporter"
exporter:
  endpoint: "https://api.honeycomb.io"
  timeout: 10s
metrics:
  prefix: domino
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Scrapes) != 2 {
		t.Fatalf("scrapes=%d", len(cfg.Scrapes))
	}
	if cfg.Scrapes[0].ResolvedSource() != "domino_keep" {
		t.Fatalf("8890 source=%q", cfg.Scrapes[0].ResolvedSource())
	}
	if cfg.Scrapes[1].ResolvedSource() != "node_exporter" {
		t.Fatalf("explicit source=%q", cfg.Scrapes[1].ResolvedSource())
	}
	if cfg.Scrapes[0].Timeout.Duration() != 5*time.Second {
		t.Fatalf("timeout=%s", cfg.Scrapes[0].Timeout.Duration())
	}
	if cfg.Scrapes[0].Interval.Duration() != 15*time.Second {
		t.Fatalf("interval default=%s", cfg.Scrapes[0].Interval.Duration())
	}
	if cfg.Scrapes[0].ResolvedPrefix() != "domino.keep" {
		t.Fatalf("prefix=%q", cfg.Scrapes[0].ResolvedPrefix())
	}
	if cfg.ScrapeInterval() != 15*time.Second {
		t.Fatalf("scrape interval=%s", cfg.ScrapeInterval())
	}
}

func TestLoadNestedSourceBlocks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mule.yaml")
	content := `
source:
  statpub:
    enabled: true
    file_path: "D:/Domino/Data/domino_stats.json"
    poll_interval: 60s
    prune: true
  prometheus:
    enabled: true
    targets:
      - name: "domino_keep"
        url: "https://127.0.0.1:8890/metrics"
        poll_interval: 15s
        timeout: 5s
        insecure: true
        username: "metrics"
        password: "password"
exporter:
  endpoint: "https://api.honeycomb.io"
  timeout: 10s
metrics:
  prefix: "domino"
  include_patterns:
    - "^HTTP\\..*"
    - "^jvm_.*"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := config.NormalizePath("D:/Domino/Data/domino_stats.json")
	if cfg.Source.FilePath != want {
		t.Fatalf("file_path=%q want %q", cfg.Source.FilePath, want)
	}
	if cfg.Source.PollInterval.Duration() != 60*time.Second {
		t.Fatalf("poll=%s", cfg.Source.PollInterval.Duration())
	}
	if !cfg.StatPubEnabled() || !cfg.PrometheusEnabled() {
		t.Fatal("both collectors should be enabled")
	}
	if len(cfg.Scrapes) != 1 {
		t.Fatalf("scrapes=%d", len(cfg.Scrapes))
	}
	s := cfg.Scrapes[0]
	if s.URL != "https://127.0.0.1:8890/metrics" {
		t.Fatalf("url=%q", s.URL)
	}
	if s.ResolvedSource() != "domino_keep" {
		t.Fatalf("source=%q", s.ResolvedSource())
	}
	if s.Interval.Duration() != 15*time.Second {
		t.Fatalf("interval=%s", s.Interval.Duration())
	}
	if !s.Insecure || s.Username != "metrics" {
		t.Fatalf("auth/tls: insecure=%v user=%q", s.Insecure, s.Username)
	}
	if !s.Allow("jvm_memory_used_bytes") {
		t.Fatal("jvm_* should inherit metrics.include_patterns")
	}
	if s.Allow("vertx_pool_ratio") {
		t.Fatal("vertx_* should be filtered by inherited include_patterns")
	}
}

func TestLoadBackendPresets(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		backend string
		url     string
		header  string
		wantVal string
	}{
		{
			name: "honeycomb",
			yaml: `
source: { file_path: "/tmp/s.json", poll_interval: 60s }
exporter:
  backend: honeycomb
  honeycomb: { api_key: "hcaik_test" }
metrics: { prefix: domino }
`,
			backend: "honeycomb",
			url:     "https://api.honeycomb.io/v1/metrics",
			header:  "x-honeycomb-team",
			wantVal: "hcaik_test",
		},
		{
			name: "grafana",
			yaml: `
source: { file_path: "/tmp/s.json", poll_interval: 60s }
exporter:
  backend: grafana
  grafana:
    endpoint: "https://otlp-gateway-prod-us-east-0.grafana.net/otlp"
    instance_id: "123456"
    token: "glc_test"
metrics: { prefix: domino }
`,
			backend: "grafana",
			url:     "https://otlp-gateway-prod-us-east-0.grafana.net/otlp/v1/metrics",
			header:  "Authorization",
			wantVal: "Basic " + base64.StdEncoding.EncodeToString([]byte("123456:glc_test")),
		},
		{
			name: "dynatrace",
			yaml: `
source: { file_path: "/tmp/s.json", poll_interval: 60s }
exporter:
  backend: dynatrace
  dynatrace:
    environment_id: "abc12345"
    api_token: "dt0c01.test"
metrics: { prefix: domino }
`,
			backend: "dynatrace",
			url:     "https://abc12345.live.dynatrace.com/api/v2/otlp/v1/metrics",
			header:  "Authorization",
			wantVal: "Api-Token dt0c01.test",
		},
		{
			name: "splunk",
			yaml: `
source: { file_path: "/tmp/s.json", poll_interval: 60s }
exporter:
  backend: splunk
  splunk:
    realm: "us0"
    access_token: "sfx_test"
metrics: { prefix: domino }
`,
			backend: "splunk",
			url:     "https://ingest.us0.signalfx.com/v2/datapoint/otlp",
			header:  "X-SF-Token",
			wantVal: "sfx_test",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mule.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Exporter.ResolvedBackend() != tc.backend {
				t.Fatalf("backend=%q", cfg.Exporter.ResolvedBackend())
			}
			u, err := cfg.Exporter.EndpointURL()
			if err != nil {
				t.Fatal(err)
			}
			if u != tc.url {
				t.Fatalf("url=%q want %q", u, tc.url)
			}
			got := ""
			for k, v := range cfg.Exporter.Headers {
				if strings.EqualFold(k, tc.header) {
					got = v
					break
				}
			}
			if got != tc.wantVal {
				t.Fatalf("header %s=%q want %q", tc.header, got, tc.wantVal)
			}
		})
	}
}

func TestLoadGrafanaBasicAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mule.yaml")
	content := `
source:
  file_path: "/tmp/stats.json"
  poll_interval: 60s
exporter:
  endpoint: "https://otlp-gateway-prod-us-east-0.grafana.net/otlp"
  path: "/v1/metrics"
  basic_auth:
    username: "123456"
    password: "glc_test"
  timeout: 10s
metrics:
  prefix: domino
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	u, err := cfg.Exporter.EndpointURL()
	if err != nil {
		t.Fatal(err)
	}
	if u != "https://otlp-gateway-prod-us-east-0.grafana.net/otlp/v1/metrics" {
		t.Fatalf("url=%q", u)
	}
	auth := cfg.Exporter.Headers["Authorization"]
	if !strings.HasPrefix(auth, "Basic ") {
		t.Fatalf("Authorization=%q", auth)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "123456:glc_test" {
		t.Fatalf("decoded=%q", raw)
	}
}

func TestLoadPruneFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mule.yaml")
	content := `
source:
  file_path: "/tmp/stats.json"
  poll_interval: 60s
  prune: false
exporter:
  endpoint: "https://api.honeycomb.io"
  timeout: 10s
metrics:
  prefix: domino
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source.PruneEnabled() {
		t.Fatal("prune: false should disable truncation")
	}
}
