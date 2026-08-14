// Package collector reads StatPub JSON and Prometheus HTTP metrics.
package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hcl/domino-mule/internal/config"
)

// Sample is one filtered numeric statistic (StatPub or Prometheus scrape).
type Sample struct {
	Key   string            // original key / Prometheus series identity
	Name  string            // normalized OTLP name
	Value float64
	Kind  Kind
	Attrs map[string]string // OTel attributes (source=statpub | domino_keep)
}

// Kind selects the OTel instrument type.
type Kind int

const (
	KindGauge Kind = iota
	KindCounter
)

func (k Kind) String() string {
	switch k {
	case KindCounter:
		return "counter"
	default:
		return "gauge"
	}
}

// Reader polls a StatPub JSON file, skipping unchanged content via mtime/size.
type Reader struct {
	cfg *config.Config

	mu           sync.Mutex
	lastMod      time.Time
	lastSize     int64
	pendingPrune bool
}

// NewReader constructs a file-backed StatPub reader.
func NewReader(cfg *config.Config) *Reader {
	return &Reader{cfg: cfg}
}

// FilePath returns the normalized StatPub JSON path.
func (r *Reader) FilePath() string {
	return config.NormalizePath(r.cfg.Source.FilePath)
}

// ReadIfChanged returns samples when the file changed since the last successful
// read. ok is false when the file is unchanged or missing (not an error).
func (r *Reader) ReadIfChanged() (samples []Sample, ok bool, err error) {
	path := config.NormalizePath(r.cfg.Source.FilePath)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat %s: %w", path, err)
	}

	mod := info.ModTime()
	size := info.Size()

	if size == 0 {
		r.mu.Lock()
		r.lastMod = mod
		r.lastSize = 0
		r.mu.Unlock()
		return nil, false, nil
	}

	r.mu.Lock()
	unchanged := !mod.After(r.lastMod) && size == r.lastSize && !r.lastMod.IsZero()
	r.mu.Unlock()
	if unchanged {
		return nil, false, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}

	raw, err := parseStatPub(data)
	if err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}

	samples = make([]Sample, 0, len(raw))
	for key, val := range raw {
		num, ok := toFloat(val)
		if !ok {
			continue
		}
		if !r.cfg.Metrics.Allow(key) {
			continue
		}
		kind := KindGauge
		if r.cfg.Metrics.IsCounter(key) {
			kind = KindCounter
		}
		samples = append(samples, Sample{
			Key:   key,
			Name:  r.cfg.Metrics.NormalizeName(key),
			Value: num,
			Kind:  kind,
			Attrs: map[string]string{"source": config.StatPubSourceAttribute},
		})
	}

	r.mu.Lock()
	r.lastMod = mod
	r.lastSize = size
	r.mu.Unlock()

	return samples, true, nil
}

// Reset clears the mtime cache so the next poll re-reads the file.
func (r *Reader) Reset() {
	r.mu.Lock()
	r.lastMod = time.Time{}
	r.lastSize = 0
	r.pendingPrune = false
	r.mu.Unlock()
}

// MarkPrunePending records that the last snapshot was exported and the StatPub
// file may now be truncated.
func (r *Reader) MarkPrunePending() {
	if !r.cfg.Source.PruneEnabled() {
		return
	}
	r.mu.Lock()
	r.pendingPrune = true
	r.mu.Unlock()
}

// PruneIfNeeded truncates the StatPub file after a successful export.
// If Domino appended more bytes since the last read, prune is skipped so the
// next poll can export the new snapshot first.
func (r *Reader) PruneIfNeeded() (pruned bool, err error) {
	if !r.cfg.Source.PruneEnabled() {
		return false, nil
	}

	r.mu.Lock()
	pending := r.pendingPrune
	lastSize := r.lastSize
	r.mu.Unlock()
	if !pending {
		return false, nil
	}

	path := config.NormalizePath(r.cfg.Source.FilePath)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			r.mu.Lock()
			r.pendingPrune = false
			r.lastMod = time.Time{}
			r.lastSize = 0
			r.mu.Unlock()
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", path, err)
	}

	if info.Size() > lastSize {
		return false, nil
	}

	if err := truncateFile(path); err != nil {
		return false, fmt.Errorf("truncate %s: %w", path, err)
	}

	info, err = os.Stat(path)
	r.mu.Lock()
	r.pendingPrune = false
	if err == nil {
		r.lastMod = info.ModTime()
		r.lastSize = info.Size()
	} else {
		r.lastMod = time.Now()
		r.lastSize = 0
	}
	r.mu.Unlock()
	return true, nil
}

func truncateFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

func parseStatPub(data []byte) (map[string]any, error) {
	root, err := decodeLastJSONObject(data)
	if err != nil {
		return nil, err
	}

	switch v := root.(type) {
	case map[string]any:
		for _, key := range []string{"Statistics", "statistics", "Stats", "metrics", "Metrics"} {
			if stats, ok := v[key].(map[string]any); ok {
				return stats, nil
			}
		}
		return v, nil
	default:
		return nil, fmt.Errorf("expected JSON object, got %T", root)
	}
}

// decodeLastJSONObject accepts a single object or concatenated/NDJSON objects
// (Domino StatPub often appends: {}{"Server.Users":1}...).
func decodeLastJSONObject(data []byte) (any, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, fmt.Errorf("empty file")
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	var last any
	found := false
	for {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if !found {
				return nil, err
			}
			break
		}
		last = v
		found = true
	}
	if !found {
		return nil, fmt.Errorf("no JSON object found")
	}
	return last, nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, false
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	case bool:
		return 0, false
	case nil:
		return 0, false
	default:
		return 0, false
	}
}
