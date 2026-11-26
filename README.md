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

Requires a bearer token, an `Idempotency-Key` header, and a JSON body:

```http
Authorization: Bearer <API_TOKEN>
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
| `401` | Missing or invalid bearer token |
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

Requirements: Go 1.24 or newer, MySQL 8 (or a compatible MariaDB version), and
an API token.
The Go version declared by the module is in `go.mod`.

For a new database, create it and apply both migrations in order:

```sh
mysql -uroot -proot -e "CREATE DATABASE IF NOT EXISTS app"
mysql -uroot -proot app < migrations/0001_schema.up.sql
mysql -uroot -proot app < migrations/0002_idempotency_claim_token.up.sql
```

For an existing database created with migration `0001`, apply only migration
`0002`.

Then run the server:

```sh
export API_TOKEN='replace-with-a-long-random-secret'
export DATABASE_URL='root:root@tcp(127.0.0.1:3306)/app?parseTime=true&interpolateParams=true&charset=utf8mb4'
export PORT=8080

go run ./cmd/server
```

| Variable | Default | Description |
|---|---|---|
| `API_TOKEN` | — | Required bearer token for `POST /transfers` |
| `DATABASE_URL` | `root:root@tcp(127.0.0.1:3306)/app?parseTime=true&interpolateParams=true&charset=utf8mb4` | MySQL DSN |
| `PORT` | `8080` | HTTP listen port |

The default database credentials are for local development only. Configure
credentials appropriate to your environment before deployment.

## Tests

```sh
go build ./... && go vet ./... && go test ./... -count=1
```

The transfer tests require MySQL at
`root:root@tcp(127.0.0.1:3306)/app` with both migrations applied. They truncate
the application tables, so use a disposable test database, not a database
containing data you need to keep.

`TestConcurrentBidirectionalStress` starts 50 A-to-B and 20 B-to-A transfers
concurrently. It checks that money is conserved, balances stay non-negative,
ledger entries reconcile, and deadlock or lock-wait errors do not escape to the
caller.

## Docker

```sh
docker build -t go-vaultcore-api:local .
export API_TOKEN='replace-with-a-long-random-secret'
docker compose up --build
```

The Compose stack starts `api` (port 8080), `mysql`, and `prometheus` (port
9090). On first initialization of the MySQL data volume, MySQL loads
both migration scripts; the API waits for MySQL's health check before starting.
An existing volume created before migration `0002` must be upgraded manually.
The API does not currently expose `/metrics`, so its Prometheus scrape target
remains down.

## CI

The workflow at `.github/workflows/ci.yml` is configured for pushes to `master`
and pull requests. It builds and vets the Go packages, applies both migrations
to its MySQL 8 service, and runs the tests. It then builds the Docker image and
scans it with Trivy.

## Current limitations

- `API_TOKEN` is shared by all callers. It authenticates access to the transfer
  endpoint but does not authorize transfers by account owner; any token holder
  can submit transfers involving any account IDs. Do not use this service to
  manage real funds without adding account-level authorization.
- Expired `processing` idempotency claims can be reclaimed after 24 hours, with
  a fencing token preventing an older operation from committing afterward.
  Completed idempotency records are not automatically purged.
