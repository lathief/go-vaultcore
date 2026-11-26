# Atomic Wallet & Ledger Service

A double-entry money-transfer API written in Go, backed by MySQL. It exposes
`POST /transfers` with atomic transactions, idempotency-key handling, and
deadlock-free row locking, plus a concurrency stress test that proves money is
conserved under 70 simultaneous bidirectional transfers.

## Features

- **Atomic transfers** — debit, credit, and ledger entries commit in a single
  MySQL transaction; non-negative balances are enforced by a DB check
  constraint (`migrations/0001_schema.up.sql`).
- **Idempotency** — an `Idempotency-Key` header makes retries safe: a replayed
  request returns the original response (200) instead of debiting twice; a key
  reused with a different payload is rejected (409).
- **Deadlock prevention** — both accounts are locked with
  `SELECT ... FOR UPDATE` in ascending-id order, so the classic A→B / B→A
  deadlock cannot occur (`internal/transfer/service.go`).
- **Ledger** — every transfer writes `debit`/`credit` rows with
  `balance_after` for a full audit trail.

## Project layout

```
cmd/server/            entrypoint: config, DB wiring, router, graceful shutdown
internal/httpapi/      HTTP handler, JSON envelope, error mapping
internal/transfer/     transfer use case, locking, idempotency, stress test
migrations/            SQL schema (accounts, ledger_entries, transfers, idempotency_keys)
prometheus/            Prometheus scrape config
Dockerfile             multi-stage build, non-root, healthcheck
docker-compose.yml     api + mysql + prometheus
.github/workflows/     CI: build/vet/test (MySQL service) + Trivy image scan
```

## API

### `GET /health`

Returns `ok`.

### `POST /transfers`

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
| `200` | Replay of a completed `Idempotency-Key` — original result returned, no double debit |
| `400` | Malformed body or validation error |
| `409` | Key still processing, key reused with a different payload, frozen account, or insufficient funds |
| `500` | Unexpected error |

Errors use one envelope shape: `{"error": {"code": "...", "message": "..."}}`.

## Running locally

Requirements: Go 1.26 and MySQL 8 (or MariaDB).

```sh
mysql -uroot -proot < migrations/0001_schema.up.sql

export DATABASE_URL='root:root@tcp(127.0.0.1:3306)/app?parseTime=true&interpolateParams=true&charset=utf8mb4'
export PORT=8080

go run ./cmd/server
```

Environment variables:

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | — | MySQL DSN (required) |
| `PORT` | `8080` | HTTP listen port |

## Tests

```sh
go build ./... && go vet ./... && go test ./... -count=1
```

The stress test (`internal/transfer/service_test.go`,
`TestConcurrentBidirectionalStress`) fires 50 concurrent A→B transfers plus 20
B→A transfers through a start gate and verifies the total system balance is
unchanged, no negative balances occur, and no deadlocks (MySQL 1213/1205)
escape to the caller. It needs the MySQL database from the step above.

## Docker

```sh
docker build -t go-vaultcore-api:local .
docker compose up --build
```

The compose stack starts `api` (port 8080), `mysql`, and `prometheus`
(port 9090). On first startup, MySQL initializes the schema from
`migrations/0001_schema.up.sql`; the API waits for MySQL's health check before
starting.

## CI

`.github/workflows/ci.yml` runs on every push and pull request:

1. Build, vet, and test against a MySQL 8 service container.
2. Docker image build with buildx cache, then a Trivy scan that fails on
   unfixed CRITICAL/HIGH vulnerabilities and reports all findings to GitHub
   code scanning via SARIF.

Saya menemukan beberapa masalah yang masih perlu diperhatikan:

### Prioritas tinggi

- **Endpoint transfer belum memakai autentikasi atau otorisasi.** Siapa pun yang bisa mengakses API berpotensi mengirim transfer jika mengetahui ID akun. Ini membuatnya belum aman untuk dipakai sebagai layanan uang sungguhan. Lihat [main.go](/C:/Users/steradian/Downloads/Projects/go-vaultcore/cmd/server/main.go) dan [transfer_handler.go](/C:/Users/steradian/Downloads/Projects/go-vaultcore/internal/httpapi/transfer_handler.go).
- **Idempotency key bisa macet selamanya.** Key diklaim dengan status `processing` sebelum transfer dimulai. Jika proses mati di tengah jalan, key itu tidak pernah dibersihkan; walaupun `expires_at` disimpan, kode tidak menggunakannya untuk memulihkan atau menghapus key. Lihat [service.go](/C:/Users/steradian/Downloads/Projects/go-vaultcore/internal/transfer/service.go).
- **Saldo tujuan bisa melampaui batas `int64`.** `dst.Balance + req.AmountCents` tidak memeriksa overflow. Jika hasilnya melewati kapasitas kolom `BIGINT`, transfer dapat gagal sebagai error internal, bukan ditolak sebagai input tidak valid.

### Ketidaksesuaian dan masalah API

- **Path health check berbeda-beda.** Aplikasi mendaftarkan `/health`, README menyebut `/healthz`, dan Dockerfile memeriksa `/healthz`. Compose sendiri memeriksa `/health`, jadi Compose dapat berjalan, tetapi image yang dijalankan langsung akan gagal health check. Lihat [Dockerfile](/C:/Users/steradian/Downloads/Projects/go-vaultcore/Dockerfile) dan [docker-compose.yml](/C:/Users/steradian/Downloads/Projects/go-vaultcore/docker-compose.yml).
- **Dokumentasi status respons tidak cocok dengan implementasi.** README menyebut validasi dan saldo tidak cukup sebagai `400`/`409`, sedangkan handler mengembalikan `422`.
- **Body JSON tambahan diterima.** Handler hanya decode satu objek. Body yang berisi objek JSON kedua setelah objek pertama tidak ditolak.
- **Pesan replay idempotency menyesatkan.** README menjanjikan respons asli, tetapi `ReplayResult` membaca saldo akun terkini. Jika ada transfer lain setelahnya, respons replay berisi saldo yang berbeda.

### Konfigurasi dan operasional

- **Kredensial database default lemah.** Compose memakai `root:root`. Cocok untuk eksperimen lokal, tetapi harus diganti sebelum penggunaan bersama atau deployment.
- **Prometheus selalu menampilkan target API sebagai down.** Konfigurasinya mencoba scrape `/metrics`, sementara aplikasi belum menyediakan endpoint itu. Komentar di `prometheus.yml` sudah menyebutkan hal ini.
- **Nama workflow CI perlu dicek.** File berada di `.github/workflows/.ci.yml` dan README merujuk ke `ci.yml`. Karena nama file diawali titik, pastikan workflow tersebut terdeteksi dan berjalan di GitHub Actions.

Saya hanya melakukan peninjauan; belum mengubah file atau menjalankan tes pada giliran ini.