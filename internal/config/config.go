// Package config loads and validates mule.yaml for the Domino Mule sidecar.
package config

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultPollInterval    = 60 * time.Second
	DefaultExportTimeout   = 10 * time.Second
	DefaultScrapeInterval  = 15 * time.Second
	DefaultScrapeTimeout   = 5 * time.Second
	DefaultStatsFile       = `D:\Mule\data\domino_stats.json`
	DefaultMetricPrefix    = "domino"
	DefaultKeepPrefix      = "domino.keep"
	DefaultServiceName     = "domino-mule"
	DefaultExporterHost    = "https://api.honeycomb.io"
	DefaultExporterPath    = "/v1/metrics"
	KeepMetricsPort        = "8890"
	KeepSourceAttribute    = "domino_keep"
	StatPubSourceAttribute = "statpub"

	BackendHoneycomb  = "honeycomb"
	BackendGrafana    = "grafana"
	BackendDynatrace  = "dynatrace"
	BackendSplunk     = "splunk"
	BackendCustom     = "custom"
)

// Config is the root mule.yaml document.
type Config struct {
	Source   SourceConfig   `yaml:"source"`
	Scrapes  []ScrapeTarget `yaml:"scrapes"`
	Exporter ExporterConfig `yaml:"exporter"`
	Metrics  MetricsConfig  `yaml:"metrics"`

	// Optional extras
	PIDFile  string         `yaml:"pid_file"`
	Resource ResourceConfig `yaml:"resource"`
}

// ScrapeTarget is one Prometheus HTTP exposition endpoint.
type ScrapeTarget struct {
	URL      string   `yaml:"url"`
	Interval Duration `yaml:"interval"`
	Timeout  Duration `yaml:"timeout"`
	// Source is the OTel attribute value for `source`. When empty, port 8890
	// is tagged as "domino_keep".
	Source string `yaml:"source"`
	// Prefix is prepended to Prometheus names (default "domino.keep" on :8890).
	Prefix          string   `yaml:"prefix"`
	Username        string   `yaml:"username"`
	Password        string   `yaml:"password"`
	Insecure        bool     `yaml:"insecure"` // skip TLS certificate verify (local Keep)
	IncludePatterns []string `yaml:"include_patterns"`
	ExcludePatterns []string `yaml:"exclude_patterns"`

	includeRE []*regexp.Regexp `yaml:"-"`
	excludeRE []*regexp.Regexp `yaml:"-"`
}

// SourceConfig locates StatPub JSON and optional nested Keep scrapes.
// Legacy fields (file_path, poll_interval, prune) remain at this level.
// Nested statpub / prometheus blocks are flattened in applyDefaults.
type SourceConfig struct {
	FilePath     string   `yaml:"file_path"`
	PollInterval Duration `yaml:"poll_interval"`
	Prune        *bool    `yaml:"prune"`

	StatPub    StatPubSource    `yaml:"statpub"`
	Prometheus PrometheusSource `yaml:"prometheus"`
}

// StatPubSource is the nested source.statpub block.
type StatPubSource struct {
	Enabled      *bool    `yaml:"enabled"`
	FilePath     string   `yaml:"file_path"`
	PollInterval Duration `yaml:"poll_interval"`
	Prune        *bool    `yaml:"prune"`
}

// PrometheusSource is the nested source.prometheus block.
type PrometheusSource struct {
	Enabled *bool              `yaml:"enabled"`
	Targets []PrometheusTarget `yaml:"targets"`
}

// PrometheusTarget is one Keep/Prometheus endpoint under source.prometheus.targets.
type PrometheusTarget struct {
	Name         string   `yaml:"name"`
	URL          string   `yaml:"url"`
	PollInterval Duration `yaml:"poll_interval"`
	Interval     Duration `yaml:"interval"`
	Timeout      Duration `yaml:"timeout"`
	Username     string   `yaml:"username"`
	Password     string   `yaml:"password"`
	Insecure     bool     `yaml:"insecure"`
	Prefix       string   `yaml:"prefix"`
	Source       string   `yaml:"source"`
}

// ExporterConfig controls the HTTP/OTLP push target.
// Set backend to honeycomb|grafana|dynatrace|splunk|custom and fill that vendor block.
type ExporterConfig struct {
	// Backend selects a vendor preset. Empty defaults to honeycomb when honeycomb
	// credentials are present, otherwise custom/raw endpoint settings.
	Backend string `yaml:"backend"`

	Endpoint string            `yaml:"endpoint"`
	Path     string            `yaml:"path"`
	Headers  map[string]string `yaml:"headers"`
	Timeout  Duration          `yaml:"timeout"`
	Insecure bool              `yaml:"insecure"`
	BasicAuth *BasicAuthConfig `yaml:"basic_auth"`

	Honeycomb *HoneycombExporter `yaml:"honeycomb"`
	Grafana   *GrafanaExporter   `yaml:"grafana"`
	Dynatrace *DynatraceExporter `yaml:"dynatrace"`
	Splunk    *SplunkExporter    `yaml:"splunk"`
}

// BasicAuthConfig is HTTP Basic credentials for the OTLP exporter.
type BasicAuthConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// HoneycombExporter is exporter.honeycomb credentials.
type HoneycombExporter struct {
	APIKey  string `yaml:"api_key"`
	Dataset string `yaml:"dataset"`
}

// GrafanaExporter is Grafana Cloud OTLP settings.
type GrafanaExporter struct {
	Endpoint   string `yaml:"endpoint"` // https://otlp-gateway-….grafana.net/otlp
	InstanceID string `yaml:"instance_id"`
	Token      string `yaml:"token"`
}

// DynatraceExporter is Dynatrace OTLP settings.
type DynatraceExporter struct {
	EnvironmentID string `yaml:"environment_id"` // abc12345 → https://abc12345.live.dynatrace.com
	Endpoint      string `yaml:"endpoint"`       // optional full host override
	APIToken      string `yaml:"api_token"`
}

// SplunkExporter is Splunk Observability Cloud OTLP settings.
type SplunkExporter struct {
	Realm        string `yaml:"realm"`         // e.g. us0 → ingest.us0.signalfx.com
	Endpoint     string `yaml:"endpoint"`      // optional full host override
	AccessToken  string `yaml:"access_token"`
}

// MetricsConfig controls OTLP naming and which Domino keys are kept.
type MetricsConfig struct {
	Prefix string `yaml:"prefix"`
	// IncludePrefixes keep keys that start with any listed string.
	IncludePrefixes []string `yaml:"include_prefixes"`
	// IncludePatterns keep keys matching any listed regex.
	IncludePatterns []string `yaml:"include_patterns"`
	ExcludePrefixes []string `yaml:"exclude_prefixes"`
	ExcludePatterns []string `yaml:"exclude_patterns"`
	// Counters lists Domino keys exported as cumulative counters (delta).
	Counters []string `yaml:"counters"`

	includeRE  []*regexp.Regexp    `yaml:"-"`
	excludeRE  []*regexp.Regexp    `yaml:"-"`
	counterSet map[string]struct{} `yaml:"-"`
}

// ResourceConfig becomes the OTel Resource attributes.
type ResourceConfig struct {
	ServiceName    string            `yaml:"service_name"`
	ServiceVersion string            `yaml:"service_version"`
	Attributes     map[string]string `yaml:"attributes"`
}

// Duration wraps time.Duration for YAML string parsing ("60s", "1m").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Duration() time.Duration { return time.Duration(d) }

// Load reads path, applies defaults, and compiles metric filter regexes.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(NormalizePath(path))
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.compile(); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// NormalizePath makes Windows and POSIX paths robust for Go I/O.
// Accepts backslashes, forward slashes, and mixed separators.
func NormalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return p
	}
	// Collapse Windows separators to slash, then OS-native via FromSlash + Clean.
	p = strings.ReplaceAll(p, `\`, `/`)
	return filepath.Clean(filepath.FromSlash(p))
}

func (c *Config) applyDefaults() {
	c.flattenNestedSource()
	c.Exporter.resolveBackend()

	if c.Source.PollInterval.Duration() <= 0 {
		c.Source.PollInterval = Duration(DefaultPollInterval)
	}
	if strings.TrimSpace(c.Source.FilePath) == "" {
		c.Source.FilePath = DefaultStatsFile
	}
	c.Source.FilePath = NormalizePath(c.Source.FilePath)

	if c.Exporter.Timeout.Duration() <= 0 {
		c.Exporter.Timeout = Duration(DefaultExportTimeout)
	}
	if strings.TrimSpace(c.Exporter.Endpoint) == "" {
		c.Exporter.Endpoint = DefaultExporterHost
	}
	if strings.TrimSpace(c.Exporter.Path) == "" {
		c.Exporter.Path = DefaultExporterPath
	}
	if strings.TrimSpace(c.Metrics.Prefix) == "" {
		c.Metrics.Prefix = DefaultMetricPrefix
	}
	if strings.TrimSpace(c.Resource.ServiceName) == "" {
		c.Resource.ServiceName = DefaultServiceName
	}
	if c.Exporter.Headers == nil {
		c.Exporter.Headers = map[string]string{}
	}
	c.Exporter.applyBasicAuth()
	if c.Resource.Attributes == nil {
		c.Resource.Attributes = map[string]string{}
	}
	if c.PIDFile != "" {
		c.PIDFile = NormalizePath(c.PIDFile)
	}
	if c.Source.Prune == nil {
		c.Source.Prune = boolPtr(true)
	}
	for i := range c.Scrapes {
		t := &c.Scrapes[i]
		t.URL = strings.TrimSpace(t.URL)
		t.Source = strings.TrimSpace(t.Source)
		t.Prefix = strings.TrimSpace(t.Prefix)
		t.Username = strings.TrimSpace(t.Username)
		if t.Timeout.Duration() <= 0 {
			t.Timeout = Duration(DefaultScrapeTimeout)
		}
		if t.Interval.Duration() <= 0 {
			t.Interval = Duration(DefaultScrapeInterval)
		}
		if t.Prefix == "" && t.ResolvedSource() == KeepSourceAttribute {
			t.Prefix = DefaultKeepPrefix
		}
		if t.Prefix == "" {
			t.Prefix = DefaultKeepPrefix
		}
		// Inherit global include_patterns when the scrape has none of its own
		// so metrics.include_patterns can filter both StatPub and Keep names.
		if len(t.IncludePatterns) == 0 && len(c.Metrics.IncludePatterns) > 0 {
			t.IncludePatterns = append([]string(nil), c.Metrics.IncludePatterns...)
		}
	}
}

func (c *Config) flattenNestedSource() {
	sp := c.Source.StatPub
	if strings.TrimSpace(sp.FilePath) != "" && strings.TrimSpace(c.Source.FilePath) == "" {
		c.Source.FilePath = sp.FilePath
	}
	if sp.PollInterval.Duration() > 0 && c.Source.PollInterval.Duration() <= 0 {
		c.Source.PollInterval = sp.PollInterval
	}
	if sp.Prune != nil && c.Source.Prune == nil {
		c.Source.Prune = sp.Prune
	}

	if c.Source.Prometheus.Enabled != nil && !*c.Source.Prometheus.Enabled {
		return
	}
	for _, tgt := range c.Source.Prometheus.Targets {
		st := scrapeFromPrometheusTarget(tgt)
		if st.URL == "" || hasScrapeURL(c.Scrapes, st.URL) {
			continue
		}
		c.Scrapes = append(c.Scrapes, st)
	}
}

func scrapeFromPrometheusTarget(t PrometheusTarget) ScrapeTarget {
	interval := t.Interval
	if interval.Duration() <= 0 {
		interval = t.PollInterval
	}
	source := strings.TrimSpace(t.Source)
	if source == "" {
		source = strings.TrimSpace(t.Name)
	}
	return ScrapeTarget{
		URL:      strings.TrimSpace(t.URL),
		Interval: interval,
		Timeout:  t.Timeout,
		Source:   source,
		Prefix:   strings.TrimSpace(t.Prefix),
		Username: strings.TrimSpace(t.Username),
		Password: t.Password,
		Insecure: t.Insecure,
	}
}

func hasScrapeURL(scrapes []ScrapeTarget, raw string) bool {
	want := strings.TrimSpace(raw)
	for _, s := range scrapes {
		if strings.TrimSpace(s.URL) == want {
			return true
		}
	}
	return false
}

func boolPtr(v bool) *bool { return &v }

func (e *ExporterConfig) resolveBackend() {
	if e.Headers == nil {
		e.Headers = map[string]string{}
	}
	backend := strings.ToLower(strings.TrimSpace(e.Backend))
	if backend == "" {
		backend = e.inferBackend()
		e.Backend = backend
	}

	switch backend {
	case BackendHoneycomb, "hny":
		e.Backend = BackendHoneycomb
		e.applyHoneycomb()
	case BackendGrafana, "grafana_cloud", "grafanacloud":
		e.Backend = BackendGrafana
		e.applyGrafana()
	case BackendDynatrace, "dt":
		e.Backend = BackendDynatrace
		e.applyDynatrace()
	case BackendSplunk, "signalfx", "o11y":
		e.Backend = BackendSplunk
		e.applySplunk()
	case BackendCustom, "":
		e.Backend = BackendCustom
	default:
		// Unknown value — leave raw endpoint/path/headers alone; validate later.
		e.Backend = backend
	}
}

func (e *ExporterConfig) inferBackend() string {
	if e.Honeycomb != nil && strings.TrimSpace(e.Honeycomb.APIKey) != "" {
		return BackendHoneycomb
	}
	if e.Grafana != nil && (strings.TrimSpace(e.Grafana.Token) != "" || strings.TrimSpace(e.Grafana.Endpoint) != "") {
		return BackendGrafana
	}
	if e.Dynatrace != nil && (strings.TrimSpace(e.Dynatrace.APIToken) != "" || strings.TrimSpace(e.Dynatrace.EnvironmentID) != "") {
		return BackendDynatrace
	}
	if e.Splunk != nil && (strings.TrimSpace(e.Splunk.AccessToken) != "" || strings.TrimSpace(e.Splunk.Realm) != "") {
		return BackendSplunk
	}
	host := strings.ToLower(strings.TrimSpace(e.Endpoint))
	switch {
	case strings.Contains(host, "honeycomb.io"):
		return BackendHoneycomb
	case strings.Contains(host, "grafana.net"):
		return BackendGrafana
	case strings.Contains(host, "dynatrace.com"):
		return BackendDynatrace
	case strings.Contains(host, "signalfx.com"), strings.Contains(host, "splunkcloud.com"):
		return BackendSplunk
	case host != "":
		return BackendCustom
	default:
		return BackendHoneycomb
	}
}

func (e *ExporterConfig) applyHoneycomb() {
	if strings.TrimSpace(e.Endpoint) == "" {
		e.Endpoint = DefaultExporterHost
	}
	if strings.TrimSpace(e.Path) == "" {
		e.Path = DefaultExporterPath
	}
	if e.Honeycomb == nil {
		return
	}
	if key := strings.TrimSpace(e.Honeycomb.APIKey); key != "" {
		e.setHeaderIfAbsent("x-honeycomb-team", key)
	}
	if ds := strings.TrimSpace(e.Honeycomb.Dataset); ds != "" {
		e.setHeaderIfAbsent("x-honeycomb-dataset", ds)
	}
}

func (e *ExporterConfig) applyGrafana() {
	if e.Grafana == nil {
		return
	}
	if ep := strings.TrimSpace(e.Grafana.Endpoint); ep != "" && strings.TrimSpace(e.Endpoint) == "" {
		e.Endpoint = strings.TrimRight(ep, "/")
	}
	if strings.TrimSpace(e.Path) == "" {
		e.Path = DefaultExporterPath
	}
	user := strings.TrimSpace(e.Grafana.InstanceID)
	pass := e.Grafana.Token
	if user != "" && pass != "" {
		if e.BasicAuth == nil {
			e.BasicAuth = &BasicAuthConfig{}
		}
		if strings.TrimSpace(e.BasicAuth.Username) == "" {
			e.BasicAuth.Username = user
		}
		if e.BasicAuth.Password == "" {
			e.BasicAuth.Password = pass
		}
	}
}

func (e *ExporterConfig) applyDynatrace() {
	if e.Dynatrace == nil {
		return
	}
	if ep := strings.TrimSpace(e.Dynatrace.Endpoint); ep != "" && strings.TrimSpace(e.Endpoint) == "" {
		e.Endpoint = strings.TrimRight(ep, "/")
	} else if env := strings.TrimSpace(e.Dynatrace.EnvironmentID); env != "" && strings.TrimSpace(e.Endpoint) == "" {
		e.Endpoint = "https://" + env + ".live.dynatrace.com"
	}
	if strings.TrimSpace(e.Path) == "" {
		e.Path = "/api/v2/otlp/v1/metrics"
	}
	if tok := strings.TrimSpace(e.Dynatrace.APIToken); tok != "" {
		if !strings.HasPrefix(strings.ToLower(tok), "api-token ") {
			tok = "Api-Token " + tok
		}
		e.setHeaderIfAbsent("Authorization", tok)
	}
}

func (e *ExporterConfig) applySplunk() {
	if e.Splunk == nil {
		return
	}
	if ep := strings.TrimSpace(e.Splunk.Endpoint); ep != "" && strings.TrimSpace(e.Endpoint) == "" {
		e.Endpoint = strings.TrimRight(ep, "/")
	} else if realm := strings.TrimSpace(e.Splunk.Realm); realm != "" && strings.TrimSpace(e.Endpoint) == "" {
		e.Endpoint = "https://ingest." + realm + ".signalfx.com"
	}
	if strings.TrimSpace(e.Path) == "" {
		e.Path = "/v2/datapoint/otlp"
	}
	if tok := strings.TrimSpace(e.Splunk.AccessToken); tok != "" {
		e.setHeaderIfAbsent("X-SF-Token", tok)
	}
}

func (e *ExporterConfig) setHeaderIfAbsent(key, value string) {
	if e.Headers == nil {
		e.Headers = map[string]string{}
	}
	for k := range e.Headers {
		if strings.EqualFold(k, key) {
			return
		}
	}
	e.Headers[key] = value
}

func (e *ExporterConfig) applyBasicAuth() {
	if e.BasicAuth == nil {
		return
	}
	user := strings.TrimSpace(e.BasicAuth.Username)
	pass := e.BasicAuth.Password
	if user == "" {
		return
	}
	if e.Headers == nil {
		e.Headers = map[string]string{}
	}
	for k := range e.Headers {
		if strings.EqualFold(k, "Authorization") {
			return
		}
	}
	token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	e.Headers["Authorization"] = "Basic " + token
}

// ResolvedBackend returns the normalized exporter backend name.
func (e ExporterConfig) ResolvedBackend() string {
	b := strings.ToLower(strings.TrimSpace(e.Backend))
	if b == "" {
		return BackendCustom
	}
	return b
}

// StatPubEnabled reports whether the StatPub file collector should run.
func (c *Config) StatPubEnabled() bool {
	if c.Source.StatPub.Enabled != nil {
		return *c.Source.StatPub.Enabled
	}
	return true
}

// PrometheusEnabled reports whether HTTP scrapes should run.
func (c *Config) PrometheusEnabled() bool {
	if c.Source.Prometheus.Enabled != nil {
		return *c.Source.Prometheus.Enabled
	}
	return len(c.Scrapes) > 0
}

// PruneEnabled reports whether the StatPub file should be truncated after export.
func (s SourceConfig) PruneEnabled() bool {
	if s.Prune == nil {
		return true
	}
	return *s.Prune
}

func (c *Config) compile() error {
	var err error
	c.Metrics.includeRE, err = compileRegexes("metrics.include_patterns", c.Metrics.IncludePatterns)
	if err != nil {
		return err
	}
	c.Metrics.excludeRE, err = compileRegexes("metrics.exclude_patterns", c.Metrics.ExcludePatterns)
	if err != nil {
		return err
	}

	c.Metrics.counterSet = make(map[string]struct{}, len(c.Metrics.Counters))
	for _, k := range c.Metrics.Counters {
		c.Metrics.counterSet[k] = struct{}{}
	}

	for i := range c.Scrapes {
		t := &c.Scrapes[i]
		field := fmt.Sprintf("scrapes[%d]", i)
		t.includeRE, err = compileRegexes(field+".include_patterns", t.IncludePatterns)
		if err != nil {
			return err
		}
		t.excludeRE, err = compileRegexes(field+".exclude_patterns", t.ExcludePatterns)
		if err != nil {
			return err
		}
	}
	return nil
}

func compileRegexes(field string, patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q: %w", field, p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func (c *Config) validate() error {
	switch c.Exporter.ResolvedBackend() {
	case BackendHoneycomb, BackendGrafana, BackendDynatrace, BackendSplunk, BackendCustom:
	default:
		return fmt.Errorf("exporter.backend must be one of honeycomb, grafana, dynatrace, splunk, custom (got %q)", c.Exporter.Backend)
	}
	if strings.TrimSpace(c.Exporter.Endpoint) == "" {
		return fmt.Errorf("exporter.endpoint is required (set exporter.backend or exporter.endpoint)")
	}
	switch c.Exporter.ResolvedBackend() {
	case BackendGrafana:
		if _, ok := headerLookup(c.Exporter.Headers, "Authorization"); !ok {
			return fmt.Errorf("exporter.grafana requires instance_id and token (or basic_auth / Authorization header)")
		}
	case BackendDynatrace:
		if _, ok := headerLookup(c.Exporter.Headers, "Authorization"); !ok {
			return fmt.Errorf("exporter.dynatrace requires api_token")
		}
	case BackendSplunk:
		if _, ok := headerLookup(c.Exporter.Headers, "X-SF-Token"); !ok {
			return fmt.Errorf("exporter.splunk requires access_token")
		}
	}
	if c.Source.PollInterval.Duration() < time.Second {
		return fmt.Errorf("source.poll_interval must be >= 1s")
	}
	if _, err := c.Exporter.EndpointURL(); err != nil {
		return err
	}
	for i, t := range c.Scrapes {
		if t.URL == "" {
			return fmt.Errorf("scrapes[%d].url is required", i)
		}
		u, err := url.Parse(t.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("scrapes[%d].url must be an absolute http(s) URL (got %q)", i, t.URL)
		}
		if t.Timeout.Duration() < time.Second {
			return fmt.Errorf("scrapes[%d].timeout must be >= 1s", i)
		}
		if t.Interval.Duration() < time.Second {
			return fmt.Errorf("scrapes[%d].interval must be >= 1s", i)
		}
	}
	return nil
}

func headerLookup(h map[string]string, key string) (string, bool) {
	for k, v := range h {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

// ScrapeInterval is the fastest configured Prometheus scrape interval.
func (c *Config) ScrapeInterval() time.Duration {
	min := time.Duration(0)
	for _, t := range c.Scrapes {
		d := t.Interval.Duration()
		if d <= 0 {
			d = DefaultScrapeInterval
		}
		if min == 0 || d < min {
			min = d
		}
	}
	if min == 0 {
		return DefaultScrapeInterval
	}
	return min
}

// ResolvedSource is the OTel `source` attribute for metrics from this target.
// Prometheus/Keep scrapes default to "domino_keep".
func (t ScrapeTarget) ResolvedSource() string {
	if t.Source != "" {
		return t.Source
	}
	return KeepSourceAttribute
}

// ResolvedPrefix is the OTLP name prefix for this scrape target.
func (t ScrapeTarget) ResolvedPrefix() string {
	if t.Prefix != "" {
		return strings.TrimSuffix(t.Prefix, ".")
	}
	if t.ResolvedSource() == KeepSourceAttribute {
		return DefaultKeepPrefix
	}
	return DefaultKeepPrefix
}

// Allow reports whether a Prometheus metric name should be exported.
// Empty include lists allow all names, then exclude lists are applied.
func (t *ScrapeTarget) Allow(name string) bool {
	if len(t.IncludePatterns) > 0 || len(t.includeRE) > 0 {
		matched := false
		for _, re := range t.includeRE {
			if re.MatchString(name) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, re := range t.excludeRE {
		if re.MatchString(name) {
			return false
		}
	}
	return true
}

// EndpointURL joins exporter.endpoint and exporter.path into a full OTLP URL.
func (e ExporterConfig) EndpointURL() (string, error) {
	base := strings.TrimSpace(e.Endpoint)
	if base == "" {
		return "", fmt.Errorf("exporter.endpoint is required")
	}

	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("exporter.endpoint must be an absolute URL (got %q)", base)
	}

	path := strings.TrimSpace(e.Path)
	if path == "" {
		path = DefaultExporterPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	// If endpoint already includes the metrics path, do not double-append.
	basePath := strings.TrimSuffix(u.Path, "/")
	if basePath != "" && (strings.HasSuffix(basePath, path) || basePath == strings.TrimSuffix(path, "/")) {
		return strings.TrimSuffix(u.String(), "/"), nil
	}

	u.Path = joinURLPath(basePath, path)
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func joinURLPath(base, rel string) string {
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		return rel
	}
	return base + rel
}

// Allow reports whether a Domino StatPub key should be exported.
// When include_prefixes and/or include_patterns are set, the key must match at least one.
func (m *MetricsConfig) Allow(key string) bool {
	hasInclude := len(m.IncludePrefixes) > 0 || len(m.includeRE) > 0
	if hasInclude {
		matched := false
		for _, p := range m.IncludePrefixes {
			if strings.HasPrefix(key, p) {
				matched = true
				break
			}
		}
		if !matched {
			for _, re := range m.includeRE {
				if re.MatchString(key) {
					matched = true
					break
				}
			}
		}
		if !matched {
			return false
		}
	}

	for _, p := range m.ExcludePrefixes {
		if strings.HasPrefix(key, p) {
			return false
		}
	}
	for _, re := range m.excludeRE {
		if re.MatchString(key) {
			return false
		}
	}
	return true
}

// IsCounter reports whether key should be exported as a cumulative counter.
func (m *MetricsConfig) IsCounter(key string) bool {
	_, ok := m.counterSet[key]
	return ok
}

// NormalizeName converts a Domino key to a dotted OTLP metric name.
// Example: prefix "domino", key "Server.Users" → "domino.server.users"
func (m *MetricsConfig) NormalizeName(key string) string {
	name := strings.ToLower(strings.TrimSpace(key))
	name = strings.ReplaceAll(name, " ", "_")
	name = strings.ReplaceAll(name, "/", ".")
	prefix := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(m.Prefix)), ".")
	if prefix == "" {
		return name
	}
	if strings.HasPrefix(name, prefix+".") {
		return name
	}
	return prefix + "." + name
}

// NormalizePromName converts a Prometheus metric name to a dotted OTLP name.
// Example: prefix "domino.keep", name "jvm_memory_used_bytes" → "domino.keep.jvm.memory.used.bytes"
func NormalizePromName(name, prefix string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, "_", ".")
	prefix = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(prefix)), ".")
	if prefix == "" {
		return name
	}
	if strings.HasPrefix(name, prefix+".") {
		return name
	}
	return prefix + "." + name
}
