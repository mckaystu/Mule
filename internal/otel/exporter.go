// Package otel wires the OpenTelemetry metric SDK and pushes Domino samples over OTLP/HTTP.
package otel

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"

	"github.com/hcl/domino-mule/internal/collector"
	"github.com/hcl/domino-mule/internal/config"
)

// Sink exports Domino samples (OTLP or stdoutmetric dry-run).
type Sink interface {
	Export(ctx context.Context, samples []collector.Sample) error
	Shutdown(ctx context.Context) error
}

// Exporter maps samples onto OTel instruments and flushes via OTLP/HTTP
// or the stdoutmetric exporter when dry-run is enabled.
type Exporter struct {
	cfg      *config.Config
	provider *sdkmetric.MeterProvider
	meter    metric.Meter
	log      *slog.Logger
	dryRun   bool

	mu           sync.Mutex
	gauges       map[string]metric.Float64Gauge
	counters     map[string]metric.Float64Counter
	lastCounters map[string]float64
}

// NewExporter builds a MeterProvider backed by OTLP/HTTP or stdoutmetric.
func NewExporter(ctx context.Context, cfg *config.Config, log *slog.Logger, dryRun bool) (*Exporter, error) {
	if log == nil {
		log = slog.Default()
	}

	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, err
	}

	reader, err := newPeriodicReader(ctx, cfg, dryRun)
	if err != nil {
		return nil, err
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
	)

	return &Exporter{
		cfg:          cfg,
		provider:     provider,
		meter:        provider.Meter("domino-mule"),
		log:          log,
		dryRun:       dryRun,
		gauges:       make(map[string]metric.Float64Gauge),
		counters:     make(map[string]metric.Float64Counter),
		lastCounters: make(map[string]float64),
	}, nil
}

func newPeriodicReader(ctx context.Context, cfg *config.Config, dryRun bool) (sdkmetric.Reader, error) {
	timeout := cfg.Exporter.Timeout.Duration()
	opts := []sdkmetric.PeriodicReaderOption{
		sdkmetric.WithInterval(24 * time.Hour),
		sdkmetric.WithTimeout(timeout),
	}

	if dryRun {
		exp, err := stdoutmetric.New(
			stdoutmetric.WithPrettyPrint(),
			stdoutmetric.WithWriter(os.Stdout),
		)
		if err != nil {
			return nil, fmt.Errorf("create stdoutmetric exporter: %w", err)
		}
		return sdkmetric.NewPeriodicReader(exp, opts...), nil
	}

	otlpOpts, err := exporterOptions(cfg)
	if err != nil {
		return nil, err
	}
	otlpExp, err := otlpmetrichttp.New(ctx, otlpOpts...)
	if err != nil {
		return nil, fmt.Errorf("create otlp metric exporter: %w", err)
	}
	return sdkmetric.NewPeriodicReader(otlpExp, opts...), nil
}

func exporterOptions(cfg *config.Config) ([]otlpmetrichttp.Option, error) {
	endpointURL, err := cfg.Exporter.EndpointURL()
	if err != nil {
		return nil, err
	}

	opts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithTimeout(cfg.Exporter.Timeout.Duration()),
		otlpmetrichttp.WithEndpointURL(endpointURL),
	}

	if len(cfg.Exporter.Headers) > 0 {
		opts = append(opts, otlpmetrichttp.WithHeaders(cfg.Exporter.Headers))
	}

	insecure := cfg.Exporter.Insecure || strings.HasPrefix(strings.ToLower(endpointURL), "http://")
	if insecure {
		opts = append(opts, otlpmetrichttp.WithInsecure())
	}

	return opts, nil
}

func buildResource(ctx context.Context, cfg *config.Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(cfg.Resource.ServiceName),
	}
	if v := strings.TrimSpace(cfg.Resource.ServiceVersion); v != "" {
		attrs = append(attrs, semconv.ServiceVersion(v))
	}
	for k, v := range cfg.Resource.Attributes {
		attrs = append(attrs, attribute.String(k, v))
	}
	return resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithHost(),
		resource.WithAttributes(attrs...),
	)
}

// Export records samples and ForceFlush-es under exporter.timeout.
func (e *Exporter) Export(parent context.Context, samples []collector.Sample) error {
	if len(samples) == 0 {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	ctx, cancel := context.WithTimeout(parent, e.cfg.Exporter.Timeout.Duration())
	defer cancel()

	recorded := 0
	for _, s := range samples {
		if err := e.recordLocked(ctx, s); err != nil {
			e.log.Warn("skip metric", "key", s.Key, "err", err)
			continue
		}
		recorded++
	}

	if err := e.provider.ForceFlush(ctx); err != nil {
		return fmt.Errorf("metric force flush: %w", err)
	}

	e.log.Info("exported metrics", "recorded", recorded, "input", len(samples), "dry_run", e.dryRun)
	return nil
}

func (e *Exporter) recordLocked(ctx context.Context, s collector.Sample) error {
	switch s.Kind {
	case collector.KindCounter:
		c, err := e.getCounter(s.Name)
		if err != nil {
			return err
		}
		id := seriesID(s)
		prev, seen := e.lastCounters[id]
		e.lastCounters[id] = s.Value
		if !seen {
			return nil
		}
		delta := s.Value - prev
		if delta < 0 {
			delta = s.Value
		}
		if delta == 0 {
			return nil
		}
		if kvs := sampleAttrs(s); len(kvs) > 0 {
			c.Add(ctx, delta, metric.WithAttributes(kvs...))
		} else {
			c.Add(ctx, delta)
		}
		return nil

	default:
		g, err := e.getGauge(s.Name)
		if err != nil {
			return err
		}
		if kvs := sampleAttrs(s); len(kvs) > 0 {
			g.Record(ctx, s.Value, metric.WithAttributes(kvs...))
		} else {
			g.Record(ctx, s.Value)
		}
		return nil
	}
}

func seriesID(s collector.Sample) string {
	if s.Key != "" {
		return s.Key
	}
	return s.Name
}

func sampleAttrs(s collector.Sample) []attribute.KeyValue {
	if len(s.Attrs) == 0 {
		return nil
	}
	kvs := make([]attribute.KeyValue, 0, len(s.Attrs))
	for k, v := range s.Attrs {
		kvs = append(kvs, attribute.String(k, v))
	}
	return kvs
}

func (e *Exporter) getGauge(name string) (metric.Float64Gauge, error) {
	if g, ok := e.gauges[name]; ok {
		return g, nil
	}
	g, err := e.meter.Float64Gauge(name,
		metric.WithDescription("Collected by domino-mule"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return nil, err
	}
	e.gauges[name] = g
	return g, nil
}

func (e *Exporter) getCounter(name string) (metric.Float64Counter, error) {
	if c, ok := e.counters[name]; ok {
		return c, nil
	}
	c, err := e.meter.Float64Counter(name,
		metric.WithDescription("Collected by domino-mule"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return nil, err
	}
	e.counters[name] = c
	return c, nil
}

// Shutdown flushes and releases the MeterProvider.
func (e *Exporter) Shutdown(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, e.cfg.Exporter.Timeout.Duration())
	defer cancel()
	if err := e.provider.Shutdown(ctx); err != nil {
		return fmt.Errorf("meter provider shutdown: %w", err)
	}
	return nil
}
