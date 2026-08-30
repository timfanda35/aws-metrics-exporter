# AWS Metrics Exporter — CLAUDE.md

> 這份文件是給 Claude（AI）看的開發指引，記錄架構決策、元件職責與開發規範。

---

## 專案概述

**AWS Metrics Exporter** 是一個 HTTP API server，負責：
1. 接收帶有查詢參數的 HTTP 請求
2. 以 AWS IAM 身份（支援 **Workload Identity Federation** 與 per-request **AssumeRole**）向 CloudWatch `GetMetricData` 查詢指標
3. 將查詢結果轉換為 Prometheus exposition format 回傳（**全部以 gauge 呈現**）
4. 支援 **AWS OAM**（Observability Access Manager）跨帳戶查詢——在 monitoring account 跑一個 exporter instance，即可讀取所有 OAM-linked source accounts 的 CloudWatch 指標

設計目標：在 **GCP Cloud Run** 上以單一 stateless container 跑起來，不必把 AWS access key 放進 container；改以 WIF（GCP ID token → AWS `AssumeRoleWithWebIdentity`）取得 AWS credentials。

---

## 目錄結構

```
aws-metrics-exporter/
├── go.mod                        # Go module 定義（Go 1.24）
├── CLAUDE.md                     # 本文件
├── Dockerfile                    # 容器映像（distroless/static, nonroot）
├── docker-compose.yaml           # 開發環境（exporter + Prometheus + LocalStack）
├── cmd/
│   └── server/
│       ├── main.go               # 程式進入點：env config、logger、--healthcheck、graceful shutdown
│       └── main_test.go
├── internal/
│   ├── auth/
│   │   └── auth.go               # WIF-first composite credential provider；GCP metadata 抓 ID token
│   ├── collector/
│   │   ├── client.go             # 窄介面 CloudWatchAPI（GetMetricData）
│   │   ├── cache.go              # (region, roleARN) keyed client cache；AssumeRole 包裝
│   │   └── collector.go          # GetMetricData → []*dto.MetricFamily
│   ├── handler/
│   │   └── handler.go            # HTTP /metrics + /healthz；參數驗證、semaphore、錯誤對應
│   └── integration/
│       └── integration_test.go   # //go:build integration 端對端測試
└── config/
    └── prometheus.yml            # Prometheus scrape 設定（開發用）
```

---

## 元件職責

### `internal/auth`
- 建立 AWS `aws.Config`
- 兩條路徑（優先 WIF）：
  1. **Workload Identity Federation**：當 `AWS_ROLE_ARN` 設定，且（a）`AWS_WEB_IDENTITY_TOKEN_FILE` 已掛載 或（b）跑在 GCP Cloud Run（透過 `K_SERVICE` 等 env var 偵測），就使用 `stscreds.NewWebIdentityRoleProvider`。GCP ID token 從 metadata server 取得（path 可由 `GCP_ID_TOKEN_URL` 覆寫用於測試）。
  2. **AWS default credential chain**：fallback 到 `config.LoadDefaultConfig`（env vars、`~/.aws/credentials`、IMDSv2、ECS、EKS）。
- 對外提供 `NewAWSConfig(ctx, cfg) (aws.Config, error)`。
- 不直接呼叫 STS——回傳的 provider 在 SDK 第一次需要 credential 時才會自動 refresh。

### `internal/collector`
- 接受 `AccountID`（必填，OAM 用）、`Region`、`Namespace`、`MetricNames`、`Statistics`、`Dimensions`、`Period`、`Interval` 等參數
- 呼叫 `cloud watch.GetMetricData`：
  - 每個 `MetricDataQuery` 都會設定 `AccountId`——這是 OAM 跨帳戶查詢的關鍵
  - `(MetricName × Statistic)` cross product，最多 500 queries / call，超過自動 chunk
  - 跟隨 `NextToken` 最多 5 hop
  - `ScanBy=TimestampDescending`，只取 latest datapoint
  - `TimeOffset`（來自 `?time_offset=`）把查詢視窗往回推：`EndTime = now - TimeOffset`，`StartTime = EndTime - Interval`，用來補償特定 namespace 已知的 CloudWatch ingest delay
  - 預設**不**在 sample 上設 `TimestampMs`，讓 Prometheus 用 scrape time 蓋章——CloudWatch 資料延遲或兩次 scrape 間資料未更新時，帶入 CloudWatch 原始 timestamp 會讓 Prometheus TSDB（要求同一 series 時間戳記嚴格遞增）判定為 "out of bounds" 或 "duplicate sample" 而丟棄。設定 `PRESERVE_CW_TIMESTAMP=true` 可還原成保留 CloudWatch 原始 timestamp 的舊行為（**不要**和 `time_offset` 一起用，兩者疊加會讓 timestamp 更舊，重新觸發同樣的拒收問題）
- 所有 metric **一律 gauge**；`statistic` 變成 label
- Client cache：key = `region + "|" + roleARN`。`roleARN` 非空時，自動以 `stscreds.NewAssumeRoleProvider` 包裝 base credentials

### `internal/handler`
- 實作 `GET /metrics` 與 `GET /healthz`
- `/metrics`：
  - 嚴格參數驗證（正則）：`account_id`（12 位數字）、`region`、`namespace`、`metric_name`（可重複）、`statistic`、`dimensions`（`Name=Value`）、`period`（1/5/10/30 或 60 的倍數）、`interval`（Go duration）、`time_offset`（Go duration，需 ≥ 0）、`role_arn`、`external_id`
  - **不允許** `account_id` 重複——一個 request 只查一個帳戶；多帳戶 fan-out 由 Prometheus relabel_configs 處理
  - Non-blocking semaphore（`MAX_CONCURRENT_SCRAPES`）→ 滿載時 429 + `Retry-After: 1`
  - Per-request `context.WithTimeout(SCRAPE_TIMEOUT)`
  - 錯誤對應表 `mapError`：辨識 `smithy.APIError.ErrorCode()` → 對應 HTTP status + log level + `Retry-After`
- `/healthz`：return 200 + `{"status":"ok"}`，**不呼叫 AWS**

### `cmd/server/main.go`
- 載入 env config + validate（一次性）
- `--healthcheck` flag 在所有其他初始化之前處理（loopback GET /healthz → exit 0/1）
- 構建 logger、`aws.Config`、`ClientCache`、`MetricsHandler`，註冊 mux
- 接收 SIGINT/SIGTERM 後 `srv.Shutdown(SHUTDOWN_GRACE)` + `cache.Close()`

---

## API 設計

### `GET /metrics`

| Query Parameter | 必填 | 預設 | 說明 |
|---|---|---|---|
| `account_id` | ✅ | — | 12 位 AWS 帳戶 ID。一律設到 `MetricDataQuery.AccountId`（OAM）。一個 request 只能一個。 |
| `region` | ✅ | — | AWS region，如 `us-east-1`。 |
| `namespace` | ✅ | — | CloudWatch namespace，如 `AWS/EC2`。 |
| `metric_name` | ✅ 可重複 | — | 一次可指定多個 metric。超過 500 自動 chunk。 |
| `statistic` | ❌ 可重複 | `Average` | `Average`/`Sum`/`Minimum`/`Maximum`/`SampleCount`。 |
| `dimensions` | ❌ 可重複 | — | `Name=Value`；套用到每個 metric_name。 |
| `period` | ❌ | `60` | CloudWatch period（秒）。 |
| `interval` | ❌ | `5m` | 查詢時間視窗（Go duration）。 |
| `time_offset` | ❌ | `0` | 把查詢視窗往回推移（Go duration，需 ≥ 0）：`EndTime = now - time_offset`。用於補償特定 namespace 的 CloudWatch ingest delay。 |
| `role_arn` | ❌ | (`DEFAULT_ROLE_ARN`) | 觸發 per-request `sts:AssumeRole`。 |
| `external_id` | ❌ | — | 僅 `role_arn` 一起用才有效。 |

**回應格式**：`text/plain; version=0.0.4; charset=utf-8`，所有 metric 為 `gauge`。

**範例**：
```
GET /metrics?account_id=123456789012&region=us-east-1&namespace=AWS/EC2&metric_name=CPUUtilization
```

### `GET /healthz`
回傳 `200 OK` + `{"status":"ok"}`。

---

## 認證設定

優先順序：

1. **WIF**：`AWS_ROLE_ARN` 設定 + (token file 已掛載 或 跑在 Cloud Run)
2. **AWS default chain**：env vars / `~/.aws/credentials` / IMDS / ECS / EKS Pod Identity

Per-request `?role_arn=`（可選）會在 base credentials 上加一層 `stscreds.NewAssumeRoleProvider`；cache 以 `(region, roleARN)` 為 key。

---

## 環境變數

| 變數名稱 | 預設 | 說明 |
|---|---|---|
| `PORT` | `8080` | HTTP server 監聽 port |
| `LOG_LEVEL` | `info` | debug/info/warn/error |
| `LOG_FORMAT` | `json` | json/text |
| `SCRAPE_TIMEOUT` | `30s` | 單次請求對 AWS 的 timeout |
| `MAX_CONCURRENT_SCRAPES` | `16` | 同時 scrape 上限，超過回 429 |
| `MAX_SERIES_PER_REQUEST` | `10000` | 單次回應最多 series 數，超過回 503 |
| `SHUTDOWN_GRACE` | `10s` | Graceful shutdown 允許時間（與 Cloud Run SIGTERM→SIGKILL 視窗一致） |
| `AWS_REGION` | — | STS region（WIF 用） |
| `AWS_ROLE_ARN` | — | WIF 目標 role |
| `AWS_WEB_IDENTITY_TOKEN_FILE` | — | 已掛載的 WIF token file 路徑 |
| `AWS_WEB_IDENTITY_TOKEN_AUDIENCE` | `sts.amazonaws.com` | GCP ID token audience |
| `GCP_ID_TOKEN_URL` | metadata-server 預設 | 測試時可覆寫 |
| `DEFAULT_ROLE_ARN` | — | `?role_arn=` 省略時的 fallback |
| `ROLE_SESSION_NAME` | `aws-metrics-exporter` | CloudTrail session 名稱 |
| `PRESERVE_CW_TIMESTAMP` | `false` | 設為 `true` 時在 sample 上保留 CloudWatch 原始 `TimestampMs`（舊行為）。**不要**和 `?time_offset=` 一起用。 |

---

## 開發指引

### 執行測試
```bash
go test ./...                                          # unit
go test -tags=integration -count=1 -race ./...         # 含 integration
go vet ./...
```

### 啟動開發環境（含 LocalStack）
```bash
docker compose up --build
# Seed LocalStack:
awslocal cloudwatch put-metric-data --namespace AWS/EC2 \
  --metric-name CPUUtilization --value 42 \
  --dimensions InstanceId=i-0abc123
```

### 本地執行（需 AWS credentials）
```bash
export AWS_REGION=us-east-1
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
go run ./cmd/server
```

### 部署到 Cloud Run（WIF）
```bash
gcloud run deploy aws-metrics-exporter \
  --image=ghcr.io/timfanda35/aws-metrics-exporter:latest \
  --region=asia-east1 \
  --service-account=aws-metrics-reader@PROJECT.iam.gserviceaccount.com \
  --no-allow-unauthenticated \
  --set-env-vars=AWS_REGION=us-east-1,AWS_ROLE_ARN=arn:aws:iam::123456789012:role/CloudRunCloudWatchReader,LOG_FORMAT=json
```

---

## 編碼規範

- 所有 exported 函式必須有 godoc 註解
- error 一律向上傳遞（不在 internal 層直接 log + swallow）
- handler 層負責 error → HTTP status code 的對應（使用 `smithy.APIError.ErrorCode()` 比對）
- 使用 `context.Context` 貫穿所有 AWS API 呼叫
- test 檔案與被測程式碼放在同一個 package（`_test.go`），使用 table-driven tests
- mock AWS API 使用窄介面 `CloudWatchAPI` 注入 hand-rolled stub（unit tests），或使用 SDK middleware 短路（integration tests）
- 不要在 collector 層引入 auth 套件——cache.go 是唯一允許跨層的接點
