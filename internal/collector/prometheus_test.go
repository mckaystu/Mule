package collector_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/common/expfmt"

	"github.com/hcl/domino-mule/internal/collector"
	"github.com/hcl/domino-mule/internal/config"
)

const promText = `
# HELP keep_up Keep process is up
# TYPE keep_up gauge
keep_up 1
# HELP jvm_memory_used_bytes JVM memory used
# TYPE jvm_memory_used_bytes gauge
jvm_memory_used_bytes{area="heap"} 1024
# HELP keep_documents_total Documents processed
# TYPE keep_documents_total counter
keep_documents_total{db="mail"} 12
`

func TestParsePromMetricsGaugesAndCounters(t *testing.T) {
	samples, err := collector.ParsePromMetrics(
		strings.NewReader(promText),
		expfmt.NewFormat(expfmt.TypeTextPlain),
		collector.PromParseOpts{Source: "domino_keep", Prefix: "domino.keep"},
	)
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]collector.Sample{}
	for _, s := range samples {
		byName[s.Name] = s
		if s.Attrs["source"] != "domino_keep" {
			t.Fatalf("%s missing source=domino_keep: %+v", s.Name, s.Attrs)
		}
	}

	up, ok := byName["domino.keep.keep.up"]
	if !ok || up.Kind != collector.KindGauge || up.Value != 1 {
		t.Fatalf("keep_up: %+v names=%v", up, keys(byName))
	}
	mem, ok := byName["domino.keep.jvm.memory.used.bytes"]
	if !ok || mem.Kind != collector.KindGauge || mem.Value != 1024 {
		t.Fatalf("jvm_memory_used_bytes: %+v", mem)
	}
	if mem.Attrs["area"] != "heap" {
		t.Fatalf("expected area=heap, got %+v", mem.Attrs)
	}
	docs, ok := byName["domino.keep.keep.documents.total"]
	if !ok || docs.Kind != collector.KindCounter || docs.Value != 12 {
		t.Fatalf("keep_documents_total: %+v", docs)
	}
}

func TestRewriteLocalhostAndHTTPSFallback(t *testing.T) {
	var sawAuth, sawPath bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path == "/metrics"
		user, pass, ok := r.BasicAuth()
		sawAuth = ok && user == "metrics" && pass == "secret"
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(promText))
	}))
	t.Cleanup(srv.Close)

	live := &config.Config{
		Scrapes: []config.ScrapeTarget{{
			URL:      srv.URL + "/metrics",
			Source:   "domino_keep",
			Prefix:   "domino.keep",
			Insecure: true,
			Username: "metrics",
			Password: "secret",
		}},
	}
	scraper := collector.NewScraper(live, nil)
	samples := scraper.Scrape(context.Background())
	if len(samples) != 3 {
		t.Fatalf("got %d samples: %+v", len(samples), samples)
	}
	if !sawPath || !sawAuth {
		t.Fatalf("path=%v auth=%v", sawPath, sawAuth)
	}
}

func TestScrapeHTTPMergesSourceAttr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(promText))
	}))
	t.Cleanup(srv.Close)

	live := &config.Config{
		Scrapes: []config.ScrapeTarget{{URL: srv.URL, Source: "domino_keep", Prefix: "domino.keep"}},
	}
	scraper := collector.NewScraper(live, nil)
	samples := scraper.Scrape(context.Background())
	if len(samples) != 3 {
		t.Fatalf("got %d samples: %+v", len(samples), samples)
	}
	for _, s := range samples {
		if s.Attrs["source"] != "domino_keep" {
			t.Fatalf("expected source=domino_keep, got %+v", s.Attrs)
		}
		if !strings.HasPrefix(s.Name, "domino.keep.") {
			t.Fatalf("expected domino.keep prefix, got %s", s.Name)
		}
	}
}

func TestResolvedSourceDefaultsToKeep(t *testing.T) {
	t.Parallel()
	got := config.ScrapeTarget{URL: "http://localhost:8890/metrics"}.ResolvedSource()
	if got != "domino_keep" {
		t.Fatalf("got %q", got)
	}
	got = config.ScrapeTarget{URL: "http://localhost:9100/metrics"}.ResolvedSource()
	if got != "domino_keep" {
		t.Fatalf("prometheus scrapes default to domino_keep, got %q", got)
	}
	got = config.ScrapeTarget{URL: "http://localhost:8890/metrics", Source: "custom"}.ResolvedSource()
	if got != "custom" {
		t.Fatalf("explicit source should win, got %q", got)
	}
}

func TestParseHistogramCountAndSum(t *testing.T) {
	text := `
# TYPE http_request_duration_seconds histogram
http_request_duration_seconds_bucket{le="1"} 2
http_request_duration_seconds_bucket{le="+Inf"} 3
http_request_duration_seconds_sum 4.5
http_request_duration_seconds_count 3
`
	samples, err := collector.ParsePromMetrics(
		strings.NewReader(text),
		expfmt.NewFormat(expfmt.TypeTextPlain),
		collector.PromParseOpts{Prefix: "domino.keep"},
	)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]float64{}
	for _, s := range samples {
		byName[s.Name] = s.Value
		if s.Kind != collector.KindCounter {
			t.Fatalf("%s kind=%s", s.Name, s.Kind)
		}
		if s.Attrs["source"] != "domino_keep" {
			t.Fatalf("default source: %+v", s.Attrs)
		}
	}
	if byName["domino.keep.http.request.duration.seconds.count"] != 3 {
		t.Fatalf("count=%v", byName)
	}
	if byName["domino.keep.http.request.duration.seconds.sum"] != 4.5 {
		t.Fatalf("sum=%v", byName)
	}
	for name := range byName {
		if strings.Contains(name, "bucket") {
			t.Fatalf("buckets should not be exported: %s", name)
		}
	}
}

func TestNormalizePromName(t *testing.T) {
	got := config.NormalizePromName("jvm_memory_used_bytes", "domino.keep")
	if got != "domino.keep.jvm.memory.used.bytes" {
		t.Fatalf("got %q", got)
	}
}

func keys(m map[string]collector.Sample) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
