# mini_pgbench

mini_pgbench puts a steady read and write load on a PostgreSQL database and records
transactions per second, errors and latency. It is one Go binary with an HTTP API
and Prometheus metrics. A separate local web GUI, `mini_pgbench_gui`, charts the
results.

The workload starts as soon as the benchmark table is ready. `POST /stop` pauses
it and `POST /start` resumes it.

The HTTP API has no authentication and no TLS. Do not expose it to a network you
do not trust. Bind it to loopback or reach it through `kubectl port-forward`.

## Quick start

You need an existing database and a role that can create a table in its default
schema. Go 1.27.3 builds the binary.

```sh
cp config.example.yaml config.yaml   # set hosts, database, user and TLS paths
go build -o bin/mini_pgbench .
./bin/mini_pgbench -config config.yaml
```

At startup mini_pgbench creates `mini_pgbench_accounts (id, balance)` if it is
missing and inserts the ids 1 to `rows` that do not exist yet. It never resets
balances or deletes rows. Do not use that table for anything else.

Each worker runs one transaction at a time, with no rate limit. A read is a
`SELECT balance` and a write is an `UPDATE ... SET balance = balance + 1` on a
random id. A failed transaction counts as one error, and that worker waits 100 ms
before the next one. There are no retries.

## Configuration

mini_pgbench reads one YAML file at startup. Unknown keys and invalid values stop
it. It ignores the `PG*` environment variables. See
[`config.example.yaml`](config.example.yaml).

| Key | Default | Meaning |
|---|---|---|
| `postgres.hosts` | required | List of hosts, passed to pgx as one multi-host connection |
| `postgres.port` | `5432` | Port for every host |
| `postgres.database`, `postgres.user` | required | The database must exist |
| `postgres.password` | empty | Only if your server asks for one |
| `postgres.sslmode` | `verify-full` | `disable`, `allow`, `prefer`, `require`, `verify-ca` or `verify-full` |
| `postgres.sslrootcert` | empty | CA file |
| `postgres.sslcert`, `postgres.sslkey` | empty | Client certificate and key for mTLS, both or neither |
| `pool.max_open`, `pool.max_idle` | `10`, `10` | Connection pool size; idle must not exceed open |
| `pool.max_lifetime`, `pool.max_idle_time` | `0s` | Go durations such as `30m`; `0s` means no limit |
| `workers` | `4` | Number of workers |
| `rows` | `100000` | Number of account rows |
| `read_weight`, `write_weight` | `80`, `20` | Relative share of reads and writes; `0` turns one off |
| `transaction_timeout` | `0s` | Deadline per transaction, including waiting for a connection; `0s` means none |
| `listen` | `:8080` | HTTP address |
| `sqlite_path` | `results.db` | File for the per-second results |

## HTTP API

```sh
curl localhost:8080/status            # {"running":true,"workers":4}
curl -X POST localhost:8080/stop      # pause; cancels the running transactions
curl -X POST localhost:8080/start     # resume
curl localhost:8080/results           # every second since startup
curl 'localhost:8080/results?limit=60' # the latest 60 seconds
curl localhost:8080/metrics           # Prometheus
```

`/results` returns a JSON array with one row per completed second, oldest first:

```json
[{"timestamp":"2026-10-21T12:00:00Z","tps":5120,"errors":0,"p50_ms":0.5,"p95_ms":0.8,"p99_ms":1.1,"total_transactions":812345,"total_errors":3}]
```

- `tps` and `errors` count the transactions that finished in that second.
- `p50_ms`, `p95_ms` and `p99_ms` are latencies of the committed transactions,
  including the wait for a pool connection. They are `null` in a second without
  commits.
- `total_transactions` and `total_errors` count everything since the process
  started, whatever `limit` is.
- `limit=n` returns the latest n rows. A `limit` that is not a positive integer
  returns 400 `{"error":"limit must be a positive integer"}`.

A transaction cancelled by `/stop` is not an error. A transaction that hits
`transaction_timeout` is.

The history lives in SQLite and starts empty at every process start. If writing
to SQLite fails, mini_pgbench stops the workers and exits with an error.

## GUI

`mini_pgbench_gui` runs on your machine and talks to the API through a port-forward.
Installing it needs only Go.

```sh
go install github.com/djosh34/mini_pgbench/mini_pgbench_gui@latest
kubectl port-forward pod/mini-pgbench 8080:8080
mini_pgbench_gui            # prints the URL to open, http://127.0.0.1:5173
```

Use `-api http://localhost:9090` if the port-forward uses another port, and
`-listen` to change the GUI address.

The GUI charts transactions and errors per second for the last 5 minutes to
24 hours, or the whole run. It shows the totals and the running state, and has
Start and Stop buttons. It asks for new rows every 2 seconds. Pause updates
stops that. The browser remembers the range and the pause setting.

To work on the GUI, run `pnpm install` and `pnpm dev` in `mini_pgbench_gui/` with
a benchmark on `localhost:8080`. Commit the output of `pnpm build` in `dist/`:
`go install` serves those files.

## Docker

```sh
docker build -t mini_pgbench .
docker run --rm -p 127.0.0.1:8080:8080 \
  --mount type=bind,src="$PWD/config.yaml",dst=/config/config.yaml,readonly \
  --mount type=bind,src="$PWD/tls",dst=/tls,readonly \
  --mount type=volume,src=mini-pgbench-results,dst=/data \
  mini_pgbench
```

The image is `scratch` with the binary and CA certificates. It runs as UID and GID
65532, so that user must be able to read the TLS files and write `/data`. Inside
the container, `localhost` is the container itself.

Every push to `main` publishes `ghcr.io/djosh34/mini_pgbench:latest` and
`:<commit sha>` for linux/amd64 and linux/arm64.

## Kubernetes

Set the hosts, database and user in [`deploy/benchmark.yaml`](deploy/benchmark.yaml),
then:

```sh
kubectl create secret generic mini-pgbench-tls \
  --from-file=ca.crt=tls/ca.crt \
  --from-file=client.crt=tls/client.crt \
  --from-file=client.key=tls/client.key
kubectl apply -f deploy/benchmark.yaml
kubectl port-forward pod/mini-pgbench 8080:8080
```

The manifest has a ConfigMap and a Pod with an `emptyDir` for the results. It
uses certificate login. If you need a password, put the config in a Secret instead
of the ConfigMap. To apply a changed config, delete and recreate the Pod.

## Prometheus

Scrape `GET /metrics`. The counters reset when the process restarts.

```promql
rate(mini_pgbench_transactions_total[1m])
rate(mini_pgbench_errors_total[1m])
histogram_quantile(0.95, sum by (le) (rate(mini_pgbench_transaction_duration_seconds_bucket[1m])))
mini_pgbench_running
go_sql_in_use_connections{db_name="postgres"}
```

The latency histogram has buckets from 50 µs to 60 s. The `go_sql_*` metrics
describe the PostgreSQL connection pool.

## Tests

```sh
go vet ./...
go test -race ./...
tests/integration.sh   # needs Docker and OpenSSL
```

Without `tests/integration.sh`, the PostgreSQL tests skip. The script starts
PostgreSQL 18.6 in Docker with generated certificates and a role without
`CREATEDB`. It then runs the PostgreSQL tests against it.
