package collector

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/hcl/domino-mule/internal/config"
)

const (
	maxScrapeBody   = 8 << 20 // 8 MiB
	scrapeUserAgent = "domino-mule"
	scrapeAccept    = "text/plain; version=0.0.4; charset=utf-8, application/openmetrics-text;q=0.5, */*;q=0.1"
)

// PromParseOpts controls Prometheus text → OTLP conversion.
type PromParseOpts struct {
	Source string
	Prefix string
	Allow  func(promName string) bool
}

// Scraper pulls Prometheus text-format metrics from HTTP endpoints.
type Scraper struct {
	cfg    *config.Config
	log    *slog.Logger
	client *http.Client
}

// NewScraper constructs an HTTP Prometheus scraper. Safe to call with no targets.
func NewScraper(cfg *config.Config, log *slog.Logger) *Scraper {
	if log == nil {
		log = slog.Default()
	}
	timeout := config.DefaultScrapeTimeout
	for _, t := range cfg.Scrapes {
		if d := t.Timeout.Duration(); d > timeout {
			timeout = d
		}
	}
	return &Scraper{
		cfg: cfg,
		log: log,
		client: &http.Client{
			Timeout: timeout,
			Transport: keepTransport(false, timeout),
		},
	}
}

func keepTransport(insecure bool, headerTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false, // Keep/Vert.x metrics are HTTP/1.1
		MaxIdleConns:          1,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: insecure, //nolint:gosec // local Keep often uses a self-signed cert
			MinVersion:         tls.VersionTLS12,
		},
	}
}

// Enabled reports whether any scrape targets are configured.
func (s *Scraper) Enabled() bool {
	return s != nil && s.cfg != nil && len(s.cfg.Scrapes) > 0
}

// Scrape fetches every target and converts Prometheus families to Samples.
// Per-target failures are logged and skipped so StatPub export can still proceed.
func (s *Scraper) Scrape(ctx context.Context) []Sample {
	if !s.Enabled() {
		return nil
	}
	out := make([]Sample, 0, 64)
	for i := range s.cfg.Scrapes {
		t := &s.cfg.Scrapes[i]
		samples, err := s.scrapeOne(ctx, t)
		if err != nil {
			s.log.Warn("prometheus scrape failed", "url", t.URL, "err", err)
			continue
		}
		s.log.Debug("prometheus scrape",
			"url", t.URL,
			"metrics", len(samples),
			"source", t.ResolvedSource(),
			"prefix", t.ResolvedPrefix(),
		)
		out = append(out, samples...)
	}
	return out
}

func (s *Scraper) scrapeOne(ctx context.Context, t *config.ScrapeTarget) ([]Sample, error) {
	timeout := t.Timeout.Duration()
	if timeout <= 0 {
		timeout = config.DefaultScrapeTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	targetURL := rewriteLocalhost(t.URL)
	client := s.client
	if t.Insecure {
		c := *s.client
		c.Timeout = timeout
		c.Transport = keepTransport(true, timeout)
		client = &c
	}

	resp, err := s.doGet(reqCtx, client, targetURL, t)
	if err != nil && isLikelyTLSMismatch(err) {
		if httpsURL, ok := rewriteHTTPToHTTPS(targetURL); ok {
			s.log.Info("http scrape closed early; retrying with TLS", "url", httpsURL)
			insecureClient := *s.client
			insecureClient.Timeout = timeout
			insecureClient.Transport = keepTransport(true, timeout)
			resp, err = s.doGet(reqCtx, &insecureClient, httpsURL, t)
		}
	}
	if err != nil {
		return nil, annotateScrapeErr(err, t)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("unexpected status %s (set scrapes[].username/password from Keep metricsAPI)", resp.Status)
		}
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	limited := &io.LimitedReader{R: resp.Body, N: maxScrapeBody + 1}
	format := expfmt.ResponseFormat(resp.Header)
	if format.FormatType() == expfmt.TypeUnknown {
		format = expfmt.NewFormat(expfmt.TypeTextPlain)
	}
	samples, err := ParsePromMetrics(limited, format, PromParseOpts{
		Source: t.ResolvedSource(),
		Prefix: t.ResolvedPrefix(),
		Allow:  t.Allow,
	})
	if err != nil {
		return nil, err
	}
	if limited.N <= 0 {
		return nil, fmt.Errorf("body exceeds %d bytes", maxScrapeBody)
	}
	return samples, nil
}

func (s *Scraper) doGet(ctx context.Context, client *http.Client, rawURL string, t *config.ScrapeTarget) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", scrapeAccept)
	req.Header.Set("User-Agent", scrapeUserAgent)
	if t.Username != "" {
		req.SetBasicAuth(t.Username, t.Password)
	}
	return client.Do(req)
}

// rewriteLocalhost maps localhost → 127.0.0.1 so Windows does not try ::1 first.
func rewriteLocalhost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := u.Hostname()
	if host != "localhost" {
		return raw
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	u.Host = "127.0.0.1:" + port
	return u.String()
}

func rewriteHTTPToHTTPS(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return "", false
	}
	u.Scheme = "https"
	return u.String(), true
}

func isLikelyTLSMismatch(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "eof") ||
		strings.Contains(msg, "tls") ||
		strings.Contains(msg, "http: server gave http response to https client")
}

func annotateScrapeErr(err error, t *config.ScrapeTarget) error {
	if err == nil {
		return nil
	}
	if t.Username == "" {
		return fmt.Errorf("%w (Keep :8890 often needs https://127.0.0.1:8890/metrics, insecure: true, and Basic auth username/password)", err)
	}
	return fmt.Errorf("%w (if Keep uses TLS, set url to https://127.0.0.1:8890/metrics and insecure: true)", err)
}

// ParsePromMetrics decodes Prometheus exposition format into OTLP samples.
func ParsePromMetrics(r io.Reader, format expfmt.Format, opts PromParseOpts) ([]Sample, error) {
	if opts.Source == "" {
		opts.Source = config.KeepSourceAttribute
	}
	if opts.Prefix == "" {
		opts.Prefix = config.DefaultKeepPrefix
	}
	dec := expfmt.NewDecoder(r, format)
	out := make([]Sample, 0, 64)
	for {
		var mf dto.MetricFamily
		err := dec.Decode(&mf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode prometheus text: %w", err)
		}
		out = append(out, metricFamilyToSamples(&mf, opts)...)
	}
	return out, nil
}

func metricFamilyToSamples(mf *dto.MetricFamily, opts PromParseOpts) []Sample {
	name := strings.TrimSpace(mf.GetName())
	if name == "" {
		return nil
	}
	if opts.Allow != nil && !opts.Allow(name) {
		return nil
	}

	out := make([]Sample, 0, len(mf.GetMetric()))
	for _, m := range mf.GetMetric() {
		labels := labelMap(m.GetLabel())
		switch mf.GetType() {
		case dto.MetricType_COUNTER:
			if s, ok := newPromSample(name, m.GetCounter().GetValue(), KindCounter, labels, opts); ok {
				out = append(out, s)
			}
		case dto.MetricType_GAUGE:
			if s, ok := newPromSample(name, m.GetGauge().GetValue(), KindGauge, labels, opts); ok {
				out = append(out, s)
			}
		case dto.MetricType_UNTYPED:
			if s, ok := newPromSample(name, m.GetUntyped().GetValue(), KindGauge, labels, opts); ok {
				out = append(out, s)
			}
		case dto.MetricType_SUMMARY:
			sum := m.GetSummary()
			if s, ok := newPromSample(name+"_count", float64(sum.GetSampleCount()), KindCounter, labels, opts); ok {
				out = append(out, s)
			}
			if s, ok := newPromSample(name+"_sum", sum.GetSampleSum(), KindCounter, labels, opts); ok {
				out = append(out, s)
			}
		case dto.MetricType_HISTOGRAM, dto.MetricType_GAUGE_HISTOGRAM:
			hist := m.GetHistogram()
			kind := KindCounter
			if mf.GetType() == dto.MetricType_GAUGE_HISTOGRAM {
				kind = KindGauge
			}
			if s, ok := newPromSample(name+"_count", float64(hist.GetSampleCount()), kind, labels, opts); ok {
				out = append(out, s)
			}
			if s, ok := newPromSample(name+"_sum", hist.GetSampleSum(), kind, labels, opts); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func newPromSample(name string, value float64, kind Kind, labels map[string]string, opts PromParseOpts) (Sample, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return Sample{}, false
	}
	attrs := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		attrs[k] = v
	}
	if opts.Source != "" {
		attrs["source"] = opts.Source
	}
	otlpName := config.NormalizePromName(name, opts.Prefix)
	return Sample{
		Key:   seriesKey(otlpName, attrs),
		Name:  otlpName,
		Value: value,
		Kind:  kind,
		Attrs: attrs,
	}, true
}

func labelMap(pairs []*dto.LabelPair) map[string]string {
	if len(pairs) == 0 {
		return nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k := p.GetName()
		if k == "" || k == "__name__" {
			continue
		}
		out[k] = p.GetValue()
	}
	return out
}

func seriesKey(name string, attrs map[string]string) string {
	if len(attrs) == 0 {
		return name
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(attrs[k])
	}
	b.WriteByte('}')
	return b.String()
}
