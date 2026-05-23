# AWS Metrics Exporter

An HTTP server that proxies Prometheus scrapes into **AWS CloudWatch GetMetricData** calls. Built to run on GCP Cloud Run (or anywhere) with one stateless container per environment.

- **`/metrics`** — one CloudWatch `GetMetricData` per scrape, converted to Prometheus exposition format.
- Multi-account first-class via **AWS Observability Access Manager (OAM)** — pass `?account_id=<source>` and let one monitoring-account identity read CloudWatch from every linked source account. Optional per-request `?role_arn=` for cross-account `sts:AssumeRole` when OAM isn't available.

Sibling projects in the same family:

- [webdevops/azure-metrics-exporter](https://github.com/webdevops/azure-metrics-exporter)
- [timfanda35/gcp-metrics-exporter](https://github.com/timfanda35/gcp-metrics-exporter)

## Features

- Stateless: every scrape is a live `GetMetricData`
- Multi-account: each request can target a different AWS account (OAM source account)
- Per-request `sts:AssumeRole` override (`?role_arn=`) when OAM is not set up
- All CloudWatch metrics emitted as Prometheus **gauges**; statistic (Average/Sum/Min/Max/SampleCount) becomes a `statistic` label
- CloudWatch observation timestamps preserved on each sample (`TimestampMs`)
- Streaming response, per-request timeout, concurrency cap
- Single static binary, distroless container
- Workload Identity Federation primary: runs on GCP Cloud Run with no static AWS keys

## Prerequisites

- Go 1.24+ (for building from source)
- AWS credentials with `cloudwatch:GetMetricData` (and `oam:ListSinks` / source-account read in the OAM monitoring account)
- Optional: Docker / Docker Compose for the dev stack (includes LocalStack)

## Quick start — local

```bash
git clone <this-repo>
cd aws-metrics-exporter

# Authenticate (any of the AWS default chain options)
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
export AWS_REGION=us-east-1

# Run
go run ./cmd/server
```

In another shell:

```bash
curl 'http://localhost:8080/healthz'

curl 'http://localhost:8080/metrics?account_id=123456789012&region=us-east-1&namespace=AWS/EC2&metric_name=CPUUtilization'
```

## Quick start — Docker Compose (with LocalStack)

```bash
docker compose up --build
# Exporter:   http://localhost:8080
# Prometheus: http://localhost:9090
# LocalStack: http://localhost:4566
```

Seed LocalStack with sample data (requires `awslocal`):

```bash
awslocal cloudwatch put-metric-data \
  --namespace AWS/EC2 \
  --metric-name CPUUtilization \
  --value 42 \
  --dimensions InstanceId=i-0abc123
```

Then scrape:

```bash
curl 'http://localhost:8080/metrics?account_id=000000000000&region=us-east-1&namespace=AWS/EC2&metric_name=CPUUtilization'
```

## Quick start — Cloud Run with Workload Identity Federation

One-time AWS setup:

1. Create an IAM OIDC provider for `https://accounts.google.com`.
2. Create an IAM role (e.g. `CloudRunCloudWatchReader`) with a trust policy that allows `sts:AssumeRoleWithWebIdentity` when `accounts.google.com:sub` equals the Cloud Run service-account email and `accounts.google.com:aud` matches `sts.amazonaws.com`.
3. Grant the role `cloudwatch:GetMetricData` (and `oam:ListSinks` if using OAM).

Deploy:

```bash
gcloud run deploy aws-metrics-exporter \
  --image=ghcr.io/timfanda35/aws-metrics-exporter:latest \
  --region=asia-east1 \
  --service-account=aws-metrics-reader@PROJECT.iam.gserviceaccount.com \
  --no-allow-unauthenticated \
  --set-env-vars=AWS_REGION=us-east-1,AWS_ROLE_ARN=arn:aws:iam::123456789012:role/CloudRunCloudWatchReader,LOG_FORMAT=json
```

The exporter fetches a GCP ID token from the Cloud Run metadata server on each STS refresh and exchanges it for AWS credentials via `AssumeRoleWithWebIdentity`. No long-lived AWS keys ever touch the container.

## API

### `GET /metrics`

| Query Parameter | Required | Default | Description |
|---|---|---|---|
| `account_id` | yes | — | 12-digit AWS account ID. Set on every `MetricDataQuery.AccountId` (OAM). One per request. |
| `region` | yes | — | AWS region, e.g. `us-east-1`. Selects the CloudWatch endpoint. |
| `namespace` | yes | — | CloudWatch namespace, e.g. `AWS/EC2`. |
| `metric_name` | yes (repeatable) | — | One CloudWatch metric per occurrence. Chunked at 500 per call. |
| `statistic` | no (repeatable) | `Average` | One of `Average`, `Sum`, `Minimum`, `Maximum`, `SampleCount`. Emitted as `statistic` label. |
| `dimensions` | no (repeatable) | — | `Name=Value`; applied to every `metric_name`. |
| `period` | no | `60` | CloudWatch aggregation period (seconds). Must be 1/5/10/30 or a multiple of 60. |
| `interval` | no | `5m` | Query window (Go duration). |
| `role_arn` | no | (`DEFAULT_ROLE_ARN`) | If set, wraps base credentials with `sts:AssumeRole`. Cached per ARN. |
| `external_id` | no | — | Honoured only with `role_arn`. |

Response: `text/plain; version=0.0.4; charset=utf-8`. Every metric is a Prometheus gauge with the latest CloudWatch datapoint (timestamp preserved as `TimestampMs`). Labels: `account_id`, `region`, `statistic`, optional `unit`, plus one label per dimension (sanitised; collisions with reserved names get a `dim_` prefix).

Example:

```bash
curl 'http://localhost:8080/metrics?\
account_id=123456789012&region=us-east-1&\
namespace=AWS/EC2&\
metric_name=CPUUtilization&metric_name=NetworkIn&\
statistic=Average&statistic=Maximum&\
dimensions=InstanceId%3Di-0abc123&\
period=300&interval=10m'
```

Response excerpt:

```
# HELP aws_ec2_cpuutilization AWS/EC2 CPUUtilization
# TYPE aws_ec2_cpuutilization gauge
aws_ec2_cpuutilization{account_id="123456789012",region="us-east-1",statistic="Average",InstanceId="i-0abc123"} 17.42 1716480000000
```

### `GET /healthz`

Liveness only. Returns `200 OK` with `{"status":"ok"}`. Does not call AWS — so a transient CloudWatch outage will not flap the pod.

### Error mapping

| Condition | HTTP | Retry-After |
|---|---|---|
| Missing required param / bad period / bad role_arn pattern | 400 | — |
| AWS `ResourceNotFoundException` | 404 | — |
| Concurrency cap hit | 429 | 1 |
| Series cap exceeded, throttling (`Throttling`, `RequestLimitExceeded`), quota | 503 | 30 |
| AWS auth / permission error (`AccessDenied`, `SignatureDoesNotMatch`, expired token) | 502 | — |
| Per-request timeout exceeded (`context.DeadlineExceeded`) | 504 | — |

## Configuration (environment variables)

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP listen port. |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. |
| `LOG_FORMAT` | `json` | `json` / `text`. |
| `SCRAPE_TIMEOUT` | `30s` | Per-request total deadline. |
| `MAX_CONCURRENT_SCRAPES` | `16` | `/metrics` semaphore size (429 when full). |
| `MAX_SERIES_PER_REQUEST` | `10000` | Cap (503 when exceeded). |
| `SHUTDOWN_GRACE` | `10s` | Matches Cloud Run SIGTERM→SIGKILL window. |
| `AWS_REGION` | — | STS region for WIF mode. |
| `AWS_ROLE_ARN` | — | WIF target role. Presence + a token source triggers WIF mode. |
| `AWS_WEB_IDENTITY_TOKEN_FILE` | — | Alternative WIF trigger (mounted token file). |
| `AWS_WEB_IDENTITY_TOKEN_AUDIENCE` | `sts.amazonaws.com` | Audience for the GCP ID token. |
| `GCP_ID_TOKEN_URL` | metadata-server default | Override for tests. |
| `DEFAULT_ROLE_ARN` | — | Fallback `role_arn` when query param omits it. |
| `ROLE_SESSION_NAME` | `aws-metrics-exporter` | Labels STS sessions in CloudTrail. |

CLI flag: `--healthcheck` does a loopback `GET /healthz` and exits 0/1. Used by Dockerfile / compose `HEALTHCHECK`.

## Authentication

Resolution order:

1. **Workload Identity Federation (WIF)** — when `AWS_ROLE_ARN` is set AND either `AWS_WEB_IDENTITY_TOKEN_FILE` is mounted or the process is running on GCP Cloud Run (auto-detected via `K_SERVICE`/`K_REVISION`/`CLOUD_RUN_JOB`). The exporter fetches a GCP ID token from the metadata server and exchanges it for AWS credentials.
2. **AWS default credential chain** — env vars (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`), `~/.aws/credentials`, IMDSv2 (EC2), ECS / EKS Pod Identity.

Optional per-request `sts:AssumeRole` wrap via `?role_arn=` (cached per ARN). Pair with `?external_id=` when the trust policy requires it.

## Prometheus scrape config

Multi-target: one Prometheus job, one `static_configs.targets` entry per `(account, region, namespace, metric)` tuple, separated by `;`. See [config/prometheus.yml](config/prometheus.yml) for the full pattern:

```yaml
scrape_configs:
  - job_name: aws-cloudwatch
    metrics_path: /metrics
    static_configs:
      - targets:
          - "123456789012;us-east-1;AWS/EC2;CPUUtilization"
          - "987654321098;us-west-2;AWS/RDS;CPUUtilization"
    relabel_configs:
      - source_labels: [__address__]
        regex: '([^;]+);([^;]+);([^;]+);(.+)'
        target_label: __param_account_id
        replacement: $1
      - source_labels: [__address__]
        regex: '([^;]+);([^;]+);([^;]+);(.+)'
        target_label: __param_region
        replacement: $2
      - source_labels: [__address__]
        regex: '([^;]+);([^;]+);([^;]+);(.+)'
        target_label: __param_namespace
        replacement: $3
      - source_labels: [__address__]
        regex: '([^;]+);([^;]+);([^;]+);(.+)'
        target_label: __param_metric_name
        replacement: $4
      - target_label: __address__
        replacement: exporter:8080
```

For dimensions (e.g. `InstanceId`), add them via `params:` on the job:

```yaml
    params:
      dimensions: ["InstanceId=i-0abc123"]
      statistic: ["Average", "Maximum"]
```

## Development

```bash
# Tidy dependencies
go mod tidy

# Unit tests
go test -count=1 -race ./...

# Integration test (real *cloudwatch.Client with middleware short-circuit — no AWS creds needed)
go test -tags=integration -count=1 -race -timeout=60s ./internal/integration/...

# Vet
go vet ./...
go vet -tags=integration ./internal/integration/...

# Build
go build -o /tmp/aws-metrics-exporter ./cmd/server
```

Project layout:

```
cmd/server           # main entry, env loading, graceful shutdown, --healthcheck
internal/auth        # WIF-first composite credential provider + GCP metadata-server ID-token retriever
internal/collector   # CloudWatch GetMetricData → Prometheus + (region, roleARN) client cache
internal/handler     # HTTP handlers (/metrics, /healthz) + semaphore + error mapping
internal/integration # end-to-end test (build tag: integration)
config/              # Prometheus scrape config (dev)
```

## Production notes

> **Security:** This service makes outbound CloudWatch calls as a privileged AWS identity. Anyone who can reach `/metrics` can read any CloudWatch metric the identity (or the OAM-linked source accounts) expose. Deploy on an internal network or behind authenticated infrastructure (Cloud Run IAM, internal LB, mesh policy) — **do not expose it to the public internet**.

- Container runs as `nonroot` on `gcr.io/distroless/static-debian12`; no shell, CA bundle included.
- `--healthcheck` flag probes `/healthz` on loopback and exits 0/1 — used by the docker-compose `HEALTHCHECK`.
- AWS SDK v2 clients are cached per `(region, roleARN)` for the lifetime of the process; restart to pick up policy changes that require a fresh STS exchange.
- CloudWatch observation timestamps are preserved on each Prometheus sample (`TimestampMs`), so CloudWatch's ~60–120s emission lag does not move data to the scrape clock.
- Only the **latest** datapoint per series is emitted per scrape; set `interval` large enough that at least one datapoint lands.

## License

[MIT](LICENSE)
