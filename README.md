# ai-o11y

Monitoring control plane for the AI-MCMP platform. It keeps the monitoring
targets, renders the configuration of the collector that runs on each target,
and answers metric queries over a Prometheus-family backend (Thanos or
VictoriaMetrics). Samples go from the collectors to the backend, never through
this service.

```
CB-Tumblebug ──(targets)──▶ ai-o11y ──(PromQL)──▶ Thanos / VictoriaMetrics
                              │                        ▲
                     collector config                  │ OTLP/HTTP
                              ▼                        │
                        VM: aio11y-otelcol ────────────┘
                        (host metrics, NVIDIA and AMD GPUs)
```

## Layout

| Path | What |
|---|---|
| `cmd/ai-o11y-server` | The service. |
| `internal/rest` | REST API (Echo), under `/aio11y`. |
| `internal/monitoring/mapping` | Target table (MariaDB) and the sync with CB-Tumblebug. |
| `internal/monitoring/source/tumblebug` | Management platform adapter for CB-Tumblebug. |
| `internal/monitoring/catalog` | Metric catalog: the fields queries name, see [docs/metric-catalog.md](docs/metric-catalog.md). |
| `internal/monitoring/query` | Renders queries to PromQL and runs them in chunks of targets. |
| `internal/monitoring/backend` | Prometheus query API client and the Thanos and VictoriaMetrics backends, selected by `AIO11Y_BACKEND`. |
| `internal/monitoring/collector` | Renders the collector configuration of a target. |
| `otelcol/` | The collector, built with the OpenTelemetry Collector Builder, and its own components (`otelcol/receiver/nvidiagpureceiver`, `otelcol/receiver/amdgpureceiver`). Not part of the service's Go module. |

## Build and run

```sh
make build            # bin/ai-o11y-server
make test
make lint
make swag             # api/docs from the handlers' godoc

make otelcol          # otelcol/_build/aio11y-otelcol (static, linux/amd64)
make otelcol-validate # render a target's configuration and validate it with the collector
make -C otelcol test  # tests of the collector's own components
```

The service starts with no configuration; each part it is not given (database,
CB-Tumblebug, metrics backend) leaves the routes that need it answering 503.
Configuration is environment variables, listed with their defaults in
[conf/template-setup.env](conf/template-setup.env).

## API

All routes are under `/aio11y` and, when `AIO11Y_AUTH_TOKEN` is set, need
`Authorization: Bearer <token>`. The Swagger UI is at `/aio11y/api/index.html`.

| Route | What |
|---|---|
| `POST /targets` | Register a target when its agent is installed. The body names the node as ai-app names a deployment target (`kind`, `nsId`, `infraId`, `nodeId`) and carries its description from CB-Tumblebug (`uid`, `name`, `os`, `gpuCount`, `gpuModel`, ...); without `uid` the description is read from CB-Tumblebug. |
| `GET /targets` | List targets; query parameters are attribute conditions (`?nsId=ns01`). |
| `GET /targets/{targetId}` · `DELETE /targets/{targetId}` | One target. |
| `GET /targets/{targetId}/collectorConfig` | The collector configuration (YAML) for the target. |
| `GET /targets/{targetId}/metrics` | The catalog as it stands for the target: what it has and what it does not, with the reason. |
| `POST /targets/sync` | Bring the targets in step with CB-Tumblebug: update listed targets, delete a missing one only when CB-Tumblebug answers it is gone. |
| `GET /metrics` | The metric catalog. |
| `POST /query` | Query one field over targets named by id or selected by attributes. |
| `GET /readyz` | Readiness. |

## Known limits

- **Collector**: the collector's own metrics are off by default
  (`AIO11Y_COLLECTOR_TELEMETRY_PORT=0`) and nothing reads them yet; set a port
  to serve them on localhost. While the backend does not answer, the collector
  keeps samples on its disk (`AIO11Y_COLLECTOR_STORAGE_DIR`, up to 256 MiB,
  newest dropped beyond that) and sends them in order once it answers again,
  also after a collector restart. A restart puts the request that was being
  sent behind the others, so a backend that refuses out-of-order samples
  (Prometheus without an out-of-order window) may refuse it. The collector
  refuses new samples while it uses more than 300 MiB of memory (memory
  limiter at 400 MiB with 100 MiB headroom).
- **GPU**: NVIDIA GPUs are read with `nvidia-smi` and AMD GPUs with `amd-smi`;
  other vendors are not read. MIG and XID fields wait for an NVML source. AMD
  GPUs give only amd-smi's own fields yet: the common `hw.gpu.*` fields and
  `hw.power` have one expression per field, NVIDIA's. `amd-smi` needs root
  (`/dev/kfd`), and an SR-IOV virtual function such as the Radeon PRO V710 on
  Azure reports little more than VRAM use. A target's metric listing cannot
  tell which fields its particular GPU leaves empty.
- **Registration**: the body of `POST /targets` is ai-app's, named in
  CB-Tumblebug's terms (`nsId`, `infraId`, `nodeId`); another management
  platform needs it reworked.
- **Sync**: a sync replaces a target's description with what CB-Tumblebug lists,
  attributes given at registration included.
- **Backend**: verified locally against Prometheus 3.16 with a Thanos 0.42
  Sidecar and Querier, and against two single-node VictoriaMetrics 1.155 behind
  vmauth ([poc/deploy](poc/deploy)). The backend must store the names
  collectors send (`system.cpu.time`): Prometheus with
  `otlp.translation_strategy: NoTranslation`, VictoriaMetrics with its default.
  A Thanos Receive rewrites the names and is not supported.
  `AIO11Y_BACKEND_PARTIAL_RESPONSE` is off for now.
- **Write failover**: with several `AIO11Y_BACKEND_WRITE_URL`s only the last
  address has the disk queue: while every address fails, samples wait there
  and go to the last address once it answers, even if an earlier one answers
  first. The collector goes back to the first address only every 10 minutes
  (the failover connector's retry interval).
- **Accounts**: basic auth only. Collectors read the write account's password
  from `AIO11Y_WRITE_PASSWORD` on their host; a collector without it still
  starts, and every write is refused. Over plain HTTP the accounts travel in
  clear text; outside a private network the backend needs TLS in front.
- **Metric catalog**: see the known limits in
  [docs/metric-catalog.md](docs/metric-catalog.md).

## Third-party licenses

ai-o11y is Apache-2.0. Its dependencies are Apache-2.0, MIT, BSD or ISC, except
these under the Mozilla Public License 2.0, used unmodified. Their source is
available from the projects below (MPL-2.0 §3.2).

| Module | Used by | Source |
|---|---|---|
| `github.com/go-sql-driver/mysql` | service (MariaDB driver) | https://github.com/go-sql-driver/mysql |
| `github.com/hashicorp/go-version` | collector (through OpenTelemetry Collector components) | https://github.com/hashicorp/go-version |
| `github.com/hashicorp/golang-lru`, `.../v2` | collector (through OpenTelemetry Collector components) | https://github.com/hashicorp/golang-lru |
