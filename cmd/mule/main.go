// Command mule is the Domino Mule sidecar entrypoint.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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

type options struct {
	configPath string
	dryRun     bool
	verbose    bool
	logPath    string
	service    string
}

func main() {
	os.Exit(run())
}

func run() int {
	opt := parseOptions()

	if handled, code := serviceCommand(opt); handled {
		return code
	}
	if runningAsWindowsService() {
		return runWindowsService(opt)
	}

	log, closer, err := openLogger(opt, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open log:", err)
		return 1
	}
	defer closer()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runForever(ctx, opt, log)
}

func parseOptions() options {
	configPath := flag.String("config", "mule.yaml", "path to mule.yaml")
	dryRun := flag.Bool("dry-run", false, "print transformed metrics to stdout via stdoutmetric (no OTLP HTTP)")
	verbose := flag.Bool("v", false, "verbose (debug) logging")
	logPath := flag.String("log", "", "log file (default: stdout; Windows service default: mule.log next to the exe)")
	service := flag.String("service", "", "Windows service command: install, uninstall, start, or stop")
	flag.Parse()
	return options{
		configPath: *configPath,
		dryRun:     *dryRun,
		verbose:    *verbose,
		logPath:    *logPath,
		service:    *service,
	}
}

func openLogger(opt options, asService bool) (*slog.Logger, func(), error) {
	logLevel := slog.LevelInfo
	if opt.verbose {
		logLevel = slog.LevelDebug
	}

	logPath := opt.logPath
	if logPath == "" && asService {
		logPath = defaultServiceLogPath()
	}

	var out io.Writer = os.Stdout
	if opt.dryRun && logPath == "" {
		out = os.Stderr
	}

	closer := func() {}
	if logPath != "" {
		logPath = config.NormalizePath(logPath)
		if dir := filepath.Dir(logPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, nil, err
			}
		}
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, nil, err
		}
		out = f
		closer = func() { _ = f.Close() }
	}

	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: logLevel})), closer, nil
}

func defaultServiceLogPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "mule.log"
	}
	return filepath.Join(filepath.Dir(exe), "mule.log")
}

func runForever(ctx context.Context, opt options, log *slog.Logger) int {
	cfg, err := config.Load(opt.configPath)
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

	sink, err := muleotel.NewExporter(ctx, cfg, log, opt.dryRun)
	if err != nil {
		log.Error("init metric exporter", "err", err)
		return 1
	}
	if opt.dryRun {
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
		"config", opt.configPath,
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
		"dry_run", opt.dryRun,
		"windows_service", runningAsWindowsService(),
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
