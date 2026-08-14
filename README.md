# Domino Mule (`domino-mule`)

Ultra-lightweight Go sidecar that bridges **HCL Domino** to OTLP-compliant APM platforms:

- Honeycomb
- Grafana Cloud
- Dynatrace
- Splunk Observability Cloud
- Any custom OTLP/HTTP collector (Grafana Alloy, OpenTelemetry Collector, etc.)

Two metric streams share one OTLP pipeline:

1. **StatPub** — local JSON dumps (e.g. `D:\Domino\Data\domino_stats.json`), tagged `source="statpub"`, names like `domino.server.users`.
2. **Domino Keep** — Prometheus text at `https://127.0.0.1:8890/metrics` (default every **15s**), tagged `source="domino_keep"`, names like `domino.keep.jvm.memory.used.bytes`.

Design goals: **under 0.1% CPU** and **under 15 MB RSS**.

**Developer:** Stuart McKay, HCLSoftware 2026

---

## Layout

```text
domino-mule/
├── cmd/mule/main.go                 # Entrypoint, --dry-run, signals
├── internal/config/config.go        # mule.yaml loader, backend presets, paths
├── internal/collector/statpub.go    # StatPub JSON reader (mtime-gated + prune)
├── internal/collector/prometheus.go # Keep Prometheus scrape (expfmt)
├── internal/otel/exporter.go        # OTel OTLP/HTTP + stdoutmetric dry-run
├── mule.yaml                        # Template (Windows + Domino Keep + backends)
├── go.mod
└── README.md
```

## Build

Requires **Go 1.22+**.

```bash
cd domino-mule
go mod tidy
go build -trimpath -ldflags="-s -w" -o bin/domino-mule ./cmd/mule
```

Cross-compile for Domino hosts:

```bash
# Linux
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/domino-mule-linux-amd64 ./cmd/mule

# Windows
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/domino-mule.exe ./cmd/mule
```

## Configure Domino StatPub (`notes.ini`)

`notes.ini` lives in the Domino data directory.

```ini
; notes.ini (server)
STATPUB_ENABLE=1
; Windows (three slashes + drive):
STATPUB_URI=file:///D:/Domino/Data/domino_stats.json
; Linux:
; STATPUB_URI=file:///dev/shm/domino_stats.json
```

Notes:

- Align StatPub’s publish interval with `source.statpub.poll_interval` (default **60s**).
- Domino and mule must share the host (or at least the file).
- StatPub **appends** JSON objects. Mule reads the **last** object, then **prunes** the file after a successful export (`prune: true` by default).

## Configure Domino Keep metrics (`:8890`)

Keep exposes Prometheus metrics on port **8890** with **HTTPS** and **Basic auth** (functional account in `keepconfig.d`).

Typical mule target:

```yaml
url: "https://127.0.0.1:8890/metrics"
insecure: true
username: "metrics"
password: "password"
```

Verify before starting mule:

```bat
curl.exe -vk -u metrics:password https://127.0.0.1:8890/metrics
```

You should see Prometheus text (`# HELP` / `# TYPE`), not `401` or `EOF`.

## Configure Domino Mule

Edit [`mule.yaml`](mule.yaml):

```yaml
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
  backend: honeycomb   # honeycomb | grafana | dynatrace | splunk | custom
  timeout: 10s
  honeycomb:
    api_key: "YOUR_HONEYCOMB_INGEST_API_KEY"

metrics:
  prefix: "domino"
  include_patterns:
    - "^HTTP\\..*"
    - "^Agent\\..*"
    - "^Database\\.DbCache.*"
    - "^http_.*"
    - "^jvm_.*"

resource:
  service_name: domino-mule
  attributes:
    domino.server: "mail01.example.com"
```

**Paths:** Prefer forward slashes (`D:/Domino/Data/...`) or escaped backslashes (`D:\\Domino\\Data\\...`).

### Key fields

| Field | Default | Purpose |
| --- | --- | --- |
| `source.statpub.file_path` | `D:\Mule\data\domino_stats.json` | StatPub JSON path |
| `source.statpub.poll_interval` | `60s` | How often to `stat` the file |
| `source.statpub.prune` | `true` | Truncate StatPub after successful export |
| `source.prometheus.targets[].url` | — | Keep / Prometheus URL |
| `source.prometheus.targets[].poll_interval` | `15s` | Keep scrape cadence |
| `source.prometheus.targets[].username` / `password` | — | Keep Basic auth |
| `source.prometheus.targets[].insecure` | `false` | Skip TLS verify (local Keep certs) |
| `exporter.backend` | `honeycomb` | Vendor preset |
| `exporter.honeycomb.api_key` | — | Honeycomb ingest key |
| `exporter.grafana.*` | — | OTLP gateway + instance_id + token |
| `exporter.dynatrace.*` | — | environment_id + api_token |
| `exporter.splunk.*` | — | realm + access_token |
| `exporter.timeout` | `10s` | OTLP push deadline |
| `metrics.include_patterns` | — | Allow-list for StatPub keys **and** Keep metric names |
| `metrics.counters` | — | Cumulative Domino keys (delta export) |

---

## OTLP backends

Set `exporter.backend` and fill **only** that vendor block.

### Honeycomb

```yaml
exporter:
  backend: honeycomb
  honeycomb:
    api_key: "YOUR_HONEYCOMB_INGEST_API_KEY"
    # dataset: "domino"
```

Resolves to `https://api.honeycomb.io/v1/metrics` with header `x-honeycomb-team`.

### Grafana Cloud

From Grafana Cloud Portal → stack → **OpenTelemetry** → **Configure**:

```yaml
exporter:
  backend: grafana
  grafana:
    endpoint: "https://otlp-gateway-prod-us-east-0.grafana.net/otlp"
    instance_id: "123456"
    token: "glc_eyJ...."
```

Explore metrics with `{service_name="domino-mule"}`.

### Dynatrace

```yaml
exporter:
  backend: dynatrace
  dynatrace:
    environment_id: "abc12345"
    api_token: "dt0c01...."
```

Resolves to `https://{env}.live.dynatrace.com/api/v2/otlp/v1/metrics` with `Authorization: Api-Token …`.

### Splunk Observability Cloud

```yaml
exporter:
  backend: splunk
  splunk:
    realm: "us0"
    access_token: "YOUR_SPLUNK_INGEST_TOKEN"
```

Resolves to `https://ingest.{realm}.signalfx.com/v2/datapoint/otlp` with header `X-SF-Token`.

### Custom (Alloy / OTel Collector / other)

```yaml
exporter:
  backend: custom
  endpoint: "http://127.0.0.1:4318"
  path: "/v1/metrics"
```

---

## Run

```bash
# Production
./bin/domino-mule -config mule.yaml

# Dry-run (stdoutmetric; logs on stderr; no outbound OTLP)
./bin/domino-mule -config mule.yaml --dry-run

# Verbose
./bin/domino-mule -config mule.yaml -v
```

Windows:

```powershell
.\bin\domino-mule.exe -config mule.yaml
.\bin\domino-mule.exe -config mule.yaml --dry-run -v
```

Graceful shutdown on **Ctrl+C** / **SIGINT** / **SIGTERM**.

Startup logs include `otlp_backend` and `otlp_endpoint`. A healthy export looks like:

```text
exported metrics recorded=… input=…
```

### Packaging on a Domino server

1. Copy `domino-mule.exe` and `mule.yaml` to a folder (e.g. `D:\Mule\data`).
2. Set `STATPUB_URI` and Keep credentials as above.
3. Put a real ingest/API token in the chosen `exporter.*` block.
4. Run from that folder so `-config mule.yaml` resolves locally.

---

## Runtime behavior

1. **StatPub gate** — `os.Stat` compares mtime + size; unchanged or empty files are skipped. Tagged `source="statpub"`.
2. **Parse & filter** — last JSON object in concatenated/NDJSON; non-numeric values dropped; include/exclude applied.
3. **Prometheus scrape** — independent ticker (default **15s**); `expfmt` parse; COUNTER → OTLP counter, GAUGE/UNTYPED → gauge; tagged `source="domino_keep"`, prefixed `domino.keep.*`.
4. **Merge** — both collectors share one MeterProvider / ForceFlush pipeline.
5. **Export** — OTLP/HTTP under `exporter.timeout`, or `stdoutmetric` when `--dry-run`.
6. **Prune** — truncate StatPub after a successful export when `prune` is true (default).

---

## License / attribution

Developed by **Stuart McKay, HCLSoftware 2026**.

Internal / project license — align with the parent repository.
