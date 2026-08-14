package collector_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hcl/domino-mule/internal/collector"
	"github.com/hcl/domino-mule/internal/config"
)

func TestReadIfChangedSkipsUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")
	payload := `{"Server.Users": 10, "Server.Name": "mail01", "Mail.X": "nope"}`
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := writeMinimalConfig(t, path)
	if err != nil {
		t.Fatal(err)
	}

	r := collector.NewReader(cfg)
	samples, ok, err := r.ReadIfChanged()
	if err != nil || !ok {
		t.Fatalf("first read: ok=%v err=%v", ok, err)
	}
	if len(samples) != 1 || samples[0].Name != "domino.server.users" {
		t.Fatalf("samples=%+v", samples)
	}
	if samples[0].Attrs["source"] != "statpub" {
		t.Fatalf("expected source=statpub, got %+v", samples[0].Attrs)
	}

	samples, ok, err = r.ReadIfChanged()
	if err != nil {
		t.Fatal(err)
	}
	if ok || samples != nil {
		t.Fatal("expected unchanged skip")
	}

	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`{"Server.Users": 11}`), 0o644); err != nil {
		t.Fatal(err)
	}
	samples, ok, err = r.ReadIfChanged()
	if err != nil || !ok || len(samples) != 1 || samples[0].Value != 11 {
		t.Fatalf("second read: ok=%v samples=%+v err=%v", ok, samples, err)
	}
}

func TestReadConcatenatedStatPubJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")
	payload := "{}{\"Server.Users\": 42}{\"Server.Users\": 43,\"Database.DbCache.Hits\": 9}"
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(t.TempDir(), "mule.yaml")
	body := `
source:
  file_path: "` + filepath.ToSlash(path) + `"
  poll_interval: 60s
exporter:
  endpoint: "https://api.honeycomb.io"
  path: "/v1/metrics"
  timeout: 10s
metrics:
  prefix: domino
  include_patterns:
    - "^Server\\.Users.*"
    - "^Database\\.DbCache.*"
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	r := collector.NewReader(cfg)
	samples, ok, err := r.ReadIfChanged()
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	byName := map[string]float64{}
	for _, s := range samples {
		byName[s.Name] = s.Value
		if s.Attrs["source"] != "statpub" {
			t.Fatalf("expected source=statpub, got %+v", s.Attrs)
		}
	}
	if byName["domino.server.users"] != 43 {
		t.Fatalf("expected last Server.Users=43, got %+v", samples)
	}
	if byName["domino.database.dbcache.hits"] != 9 {
		t.Fatalf("expected DbCache hits, got %+v", samples)
	}
}

func TestPruneTruncatesAfterExport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")
	payload := "{}{\"Server.Users\": 1}{\"Server.Users\": 2}"
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := writeMinimalConfig(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Source.PruneEnabled() {
		t.Fatal("expected prune default true")
	}

	r := collector.NewReader(cfg)
	samples, ok, err := r.ReadIfChanged()
	if err != nil || !ok || len(samples) != 1 || samples[0].Value != 2 {
		t.Fatalf("read: ok=%v samples=%+v err=%v", ok, samples, err)
	}

	r.MarkPrunePending()
	pruned, err := r.PruneIfNeeded()
	if err != nil || !pruned {
		t.Fatalf("prune: pruned=%v err=%v", pruned, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("expected truncated file, size=%d", info.Size())
	}

	samples, ok, err = r.ReadIfChanged()
	if err != nil {
		t.Fatal(err)
	}
	if ok || samples != nil {
		t.Fatal("empty pruned file should be skipped")
	}
}

func TestPruneSkipsWhenFileGrew(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")
	if err := os.WriteFile(path, []byte(`{"Server.Users": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := writeMinimalConfig(t, path)
	if err != nil {
		t.Fatal(err)
	}
	r := collector.NewReader(cfg)
	if _, ok, err := r.ReadIfChanged(); err != nil || !ok {
		t.Fatalf("read: ok=%v err=%v", ok, err)
	}

	grew := `{"Server.Users": 1}{"Server.Users": 99}`
	if err := os.WriteFile(path, []byte(grew), 0o644); err != nil {
		t.Fatal(err)
	}

	r.MarkPrunePending()
	pruned, err := r.PruneIfNeeded()
	if err != nil {
		t.Fatal(err)
	}
	if pruned {
		t.Fatal("must not truncate when Domino appended since the last read")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != grew {
		t.Fatalf("file was modified: %s", data)
	}
}

func TestPruneDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")
	payload := `{"Server.Users": 7}`
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(t.TempDir(), "mule.yaml")
	body := `
source:
  file_path: "` + filepath.ToSlash(path) + `"
  poll_interval: 60s
  prune: false
exporter:
  endpoint: "https://api.honeycomb.io"
  path: "/v1/metrics"
  timeout: 10s
metrics:
  prefix: domino
  include_patterns:
    - "^Server\\.Users.*"
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	r := collector.NewReader(cfg)
	if _, ok, err := r.ReadIfChanged(); err != nil || !ok {
		t.Fatalf("read: ok=%v err=%v", ok, err)
	}
	r.MarkPrunePending()
	pruned, err := r.PruneIfNeeded()
	if err != nil || pruned {
		t.Fatalf("disabled prune: pruned=%v err=%v", pruned, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != payload {
		t.Fatalf("file should be untouched: %s", data)
	}
}

func writeMinimalConfig(t *testing.T, statsPath string) (*config.Config, error) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mule.yaml")
	body := `
source:
  file_path: "` + filepath.ToSlash(statsPath) + `"
  poll_interval: 60s
exporter:
  endpoint: "https://api.honeycomb.io"
  path: "/v1/metrics"
  timeout: 10s
metrics:
  prefix: domino
  include_patterns:
    - "^Server\\.Users.*"
  exclude_patterns:
    - '(?i)\.name$'
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		return nil, err
	}
	return config.Load(cfgPath)
}
