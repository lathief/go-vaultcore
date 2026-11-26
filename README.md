# Atomic Wallet & Ledger Service

A double-entry money-transfer API written in Go and backed by MySQL. It exposes
`POST /transfers` with database transactions, idempotency-key handling, ordered
account-row locking, and a concurrency test for balance conservation.

## Features

- **Atomic transfers** — account debits and credits, ledger entries, and
  completion of an idempotency key commit in one MySQL transaction.
  Non-negative balances are enforced by a database check constraint
  (`migrations/0001_schema.up.sql`).
- **Idempotency** — an `Idempotency-Key` header prevents a completed request
  from being executed twice. Reusing the key with a different payload returns
  `409 Conflict`.
- **Ordered row locking** — both accounts are selected for update in ascending
  ID order to avoid the classic opposite-direction transfer deadlock.
- **Ledger** — each transfer writes debit and credit entries with the resulting
  account balances.

## Project layout

```
cmd/server/            entrypoint: config, DB wiring, router, graceful shutdown
internal/httpapi/      HTTP handler, JSON envelope, error mapping
internal/transfer/     transfer use case, locking, idempotency, tests
migrations/            SQL schema (accounts, ledger_entries, transfers, idempotency_keys)
prometheus/            Prometheus scrape config
Dockerfile             multi-stage build, non-root, healthcheck
docker-compose.yml     api + mysql + prometheus
.github/workflows/     CI build, vet, tests, and Trivy image scan
```

## API

### `GET /health`

Returns the plain-text response `ok`.

### `POST /transfers`

Requires an `Idempotency-Key` header and a JSON body:

```http
Idempotency-Key: transfer-123
Content-Type: application/json
```

```json
{
  "source_account_id": 1,
  "destination_account_id": 2,
  "amount_cents": 5000
}
```

| Response | Meaning |
|---|---|
| `201` | Transfer created |
| `200` | Completed request replayed; no second transfer is executed |
| `400` | Malformed JSON or an unknown JSON field |
| `409` | Idempotency key is in progress or was reused with a different payload |
| `422` | Validation, account, currency, or insufficient-funds error |
| `500` | Unexpected server error |

A successful response contains `transfer_id`, `source_balance_cents`, and
`target_balance_cents`. A replay returns the original transfer ID, but the
balance fields are read from the accounts at replay time and may differ from
the first response.

Errors produced by the transfer handler use this envelope:
`{"error": {"code": "...", "message": "..."}}`.

## Running locally

Requirements: Go 1.24 or newer and MySQL 8 (or a compatible MariaDB version).
The Go version declared by the module is in `go.mod`.

Create the database and apply the schema:

```sh
mysql -uroot -proot -e "CREATE DATABASE IF NOT EXISTS app"
mysql -uroot -proot app < migrations/0001_schema.up.sql
```

Then run the server:

```sh
export DATABASE_URL='root:root@tcp(127.0.0.1:3306)/app?parseTime=true&interpolateParams=true&charset=utf8mb4'
export PORT=8080

go run ./cmd/server
```

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | `root:root@tcp(127.0.0.1:3306)/app?parseTime=true&interpolateParams=true&charset=utf8mb4` | MySQL DSN |
| `PORT` | `8080` | HTTP listen port |

The default database credentials are for local development only. Configure
credentials appropriate to your environment before deployment.

## Tests

```sh
go build ./... && go vet ./... && go test ./... -count=1
```

The transfer tests require MySQL at
`root:root@tcp(127.0.0.1:3306)/app` with the schema applied. They truncate the
application tables, so use a disposable test database, not a database containing
data you need to keep.

`TestConcurrentBidirectionalStress` starts 50 A-to-B and 20 B-to-A transfers
concurrently. It checks that money is conserved, balances stay non-negative,
ledger entries reconcile, and deadlock or lock-wait errors do not escape to the
caller.

## Docker

```sh
docker build -t go-vaultcore-api:local .
docker compose up --build
```

The Compose stack starts `api` (port 8080), `mysql`, and `prometheus` (port
9090). On first initialization of the MySQL data volume, MySQL loads
`migrations/0001_schema.up.sql`; the API waits for MySQL's health check before
starting. The API does not currently expose `/metrics`, so its Prometheus
scrape target remains down.

## CI

The workflow at `.github/workflows/.ci.yml` is configured for pushes to `main`
and pull requests. It builds, vets, and tests against a MySQL 8 service, then
builds the Docker image and scans it with Trivy.

The workflow currently does not apply `migrations/0001_schema.up.sql` to its
MySQL service before running the database-backed tests. Those tests require the
schema, so the CI database setup must apply the migration for the test job to
pass.

## Current limitations

- The API does not implement authentication or authorization. Do not expose it
  to untrusted callers or use it to manage real funds as-is.
- Idempotency keys store a 24-hour `expires_at`, but the service does not
  currently reclaim expired `processing` keys. A process interruption can
  therefore leave a key unavailable for retry.
