// Command mule is the Domino Mule sidecar entrypoint.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hcl/domino-mule/internal/collector"
	"github.com/hcl/domino-mule/internal/config"
	muleotel "github.com/hcl/domino-mule/internal/otel"
)

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "mule.yaml", "path to mule.yaml")
	dryRun := flag.Bool("dry-run", false, "print transformed metrics to stdout via stdoutmetric (no OTLP HTTP)")
	verbose := flag.Bool("v", false, "verbose (debug) logging")
	flag.Parse()

	logLevel := slog.LevelInfo
	if *verbose {
		logLevel = slog.LevelDebug
	}
	logOut := os.Stdout
	if *dryRun {
		logOut = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: logLevel}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("load config", "err", err)
		return 1
	}

	if cfg.PIDFile != "" {
		if err := writePID(cfg.PIDFile); err != nil {
			log.Error("write pid file", "err", err)
			return 1
		}
		defer func() { _ = os.Remove(cfg.PIDFile) }()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sink, err := muleotel.NewExporter(ctx, cfg, log, *dryRun)
	if err != nil {
		log.Error("init metric exporter", "err", err)
		return 1
	}
	if *dryRun {
		log.Info("dry-run enabled; metrics will print to stdout via stdoutmetric (no OTLP)")
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Exporter.Timeout.Duration())
		defer cancel()
		if err := sink.Shutdown(shutdownCtx); err != nil {
			log.Error("exporter shutdown", "err", err)
		}
	}()

	reader := collector.NewReader(cfg)
	scraper := collector.NewScraper(cfg, log)

	endpointURL, _ := cfg.Exporter.EndpointURL()
	log.Info("domino-mule started",
		"config", *configPath,
		"stats_file", cfg.Source.FilePath,
		"statpub", cfg.StatPubEnabled(),
		"poll_interval", cfg.Source.PollInterval.Duration().String(),
		"prune", cfg.Source.PruneEnabled(),
		"prometheus", cfg.PrometheusEnabled(),
		"scrapes", len(cfg.Scrapes),
		"scrape_interval", cfg.ScrapeInterval().String(),
		"export_timeout", cfg.Exporter.Timeout.Duration().String(),
		"otlp_endpoint", endpointURL,
		"otlp_backend", cfg.Exporter.ResolvedBackend(),
		"dry_run", *dryRun,
	)
	if cfg.PrometheusEnabled() {
		for _, t := range cfg.Scrapes {
			log.Info("prometheus scrape target",
				"url", t.URL,
				"interval", t.Interval.Duration().String(),
				"timeout", t.Timeout.Duration().String(),
				"source", t.ResolvedSource(),
				"prefix", t.ResolvedPrefix(),
			)
		}
	}

	var statC <-chan time.Time
	if cfg.StatPubEnabled() {
		if err := collectStatPub(ctx, log, reader, sink); err != nil {
			log.Warn("initial statpub poll", "err", err)
		}
		statTicker := time.NewTicker(cfg.Source.PollInterval.Duration())
		defer statTicker.Stop()
		statC = statTicker.C
	}

	var scrapeC <-chan time.Time
	if cfg.PrometheusEnabled() && scraper.Enabled() {
		if err := collectProm(ctx, log, scraper, sink); err != nil {
			log.Warn("initial prometheus scrape", "err", err)
		}
		scrapeTicker := time.NewTicker(cfg.ScrapeInterval())
		defer scrapeTicker.Stop()
		scrapeC = scrapeTicker.C
	}

	for {
		select {
		case <-ctx.Done():
			log.Info("shutdown signal received")
			return 0
		case <-statC:
			if err := collectStatPub(ctx, log, reader, sink); err != nil {
				log.Warn("statpub poll", "err", err)
			}
		case <-scrapeC:
			if err := collectProm(ctx, log, scraper, sink); err != nil {
				log.Warn("prometheus scrape", "err", err)
			}
		}
	}
}

func collectStatPub(ctx context.Context, log *slog.Logger, reader *collector.Reader, sink muleotel.Sink) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	samples, changed, err := reader.ReadIfChanged()
	if err != nil {
		return err
	}
	if changed {
		log.Debug("stats file changed", "metrics", len(samples))
		if err := sink.Export(ctx, samples); err != nil {
			return err
		}
		reader.MarkPrunePending()
	} else {
		log.Debug("stats file unchanged")
	}

	pruned, err := reader.PruneIfNeeded()
	if err != nil {
		log.Warn("prune stats file", "file", reader.FilePath(), "err", err)
		return nil
	}
	if pruned {
		log.Info("pruned stats file", "file", reader.FilePath())
	}
	return nil
}

func collectProm(ctx context.Context, log *slog.Logger, scraper *collector.Scraper, sink muleotel.Sink) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	samples := scraper.Scrape(ctx)
	if len(samples) == 0 {
		return nil
	}
	log.Debug("prometheus scrape merged", "metrics", len(samples))
	return sink.Export(ctx, samples)
}

func writePID(path string) error {
	path = config.NormalizePath(path)
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
}
