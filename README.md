# mini_pgbench

A small, deliberately unsecured PostgreSQL load tool: one Go binary, one multi-host
cluster connection pool, fixed workers, JSON controls and Prometheus metrics. It
starts **paused**. There is no UI, authentication, application retry, rate limiter,
script engine or database creation.

## Prerequisites

- An **existing PostgreSQL database** and role allowed to create/use
  `mini_pgbench_accounts` in its default schema. `CREATEDB` is not required.
- Go **1.27.2** for local builds, or Docker / Kubernetes for containers.
- A writable directory for disposable SQLite results; optional PostgreSQL TLS files.

Initialization creates `mini_pgbench_accounts(id bigint PRIMARY KEY, balance bigint)`
and inserts missing IDs from 1 through `rows`, without resetting balances or
removing existing rows. This table belongs to this tool: do not modify it during a
run or share it with an application. Initialization is not benchmark traffic.

## Configuration and local usage

```sh
cp config.example.yaml config.yaml
# Edit connection settings and TLS paths; use sqlite_path: ./results.db locally.
go build -o bin/mini_pgbench .
./bin/mini_pgbench -config config.yaml
```

One YAML document is read once. Unknown fields and invalid counts, weights,
durations or referenced TLS files fail startup. There are no application-setting
environment overrides or live reloads. The full example is
[`config.example.yaml`](config.example.yaml).

| Setting | Default / meaning |
|---|---|
| `postgres.hosts` | Required hostname/address array; passed to pgx as its ordinary host list, not separate runners |
| `postgres.port` | `5432`, common to all hosts |
| `postgres.database`, `postgres.user` | Required; database must already exist |
| `postgres.password` | Empty; supply only if your PostgreSQL authentication requires it |
| `postgres.sslmode` | `verify-full`; standard pgx `disable`, `allow`, `prefer`, `require`, `verify-ca`, `verify-full` are supported |
| `postgres.sslrootcert` | Optional CA file path |
| `postgres.sslcert`, `postgres.sslkey` | Optional client certificate/key paths, supplied together for mTLS |
| `pool.max_open`, `pool.max_idle` | `10`, `10`; max open must be positive; idle must be between zero and max open |
| `pool.max_lifetime`, `pool.max_idle_time` | `0s` (unlimited); Go durations, e.g. `30m`, `5m` |
| `workers` | `4`, positive fixed worker count |
| `rows` | `100000`, positive configured ID range |
| `read_weight`, `write_weight` | `80`, `20`; nonnegative, at least one positive; zero disables that operation |
| `transaction_timeout` | `0s` = no imposed deadline; otherwise covers acquisition through commit |
| `listen` | `:8080` |
| `sqlite_path` | `results.db`; filesystem path, not a SQLite URI or in-memory database |

Each worker chooses an operation by relative weight and a random configured row.
Reads execute `SELECT balance`; writes execute `UPDATE ... SET balance = balance + 1`.
Both use an explicit GORM transaction, including commit. There is one operation in
flight per worker, without throttling. A failed transaction is counted once,
followed by a context-cancellable **100ms** delay before a new independent
transaction. There are **no application retries**. Normal pgx pool reconnection
behavior is unchanged. TLS verification policy is your choice: insecure modes
can permit interception. Configure the server's `pg_hba.conf` to require client
certificates if you need mTLS.

## HTTP API

All application responses, including errors, are JSON; only `/metrics` uses
Prometheus exposition. A result stream interrupted after headers is aborted rather
than returning a misleading complete result.

```sh
curl -X POST localhost:8080/start   # idempotent
curl localhost:8080/status
curl -X POST localhost:8080/stop    # cancels and joins workers; idempotent
curl localhost:8080/results
curl localhost:8080/metrics
```

`/status` returns `running` and configured `workers`. Starts/stops are serialized;
a new start uses a fresh cancellable context. Explicit stop cancellation is not
counted as a database error; transaction deadlines while running are errors.
SIGINT/SIGTERM also cancel and join workers before closing storage.

`/results` streams every **completed** second in chronological order:

```json
[{"timestamp":"2026-10-19T12:00:00Z","tps":123,"errors":0,"p50_ms":0.7,"p95_ms":1.2,"p99_ms":2.1}]
```

The timestamp is the start of a one-second process-relative window. Outcomes belong
to their completion window; successful latency includes pool acquisition through
commit. HdrHistogram estimates percentiles at microsecond resolution with three
significant digits. Failed transactions do not contribute latency samples. Paused
and initialization seconds have zero counts; zero-success windows have JSON
`null` percentiles. The current incomplete second is not returned.

History is reset on **every process start**, even when the same volume survives a
container restart. All completed seconds are retained for that process lifetime;
there is no persistent history, pagination or session model. SQLite has one summary
writer; a write failure logs an error, stops workers and **exits nonzero**. Large or
slow result downloads can delay the SQLite writer; this is a small test tool, not a
history service.

**Never expose the HTTP port to an untrusted network.** It has no authentication or
TLS. Bind to loopback locally or use `kubectl port-forward`.

## Docker

```sh
docker build -t mini_pgbench .
docker run --rm -p 127.0.0.1:8080:8080 \
  --mount type=bind,src="$PWD/config.yaml",dst=/config/config.yaml,readonly \
  --mount type=bind,src="$PWD/tls",dst=/tls,readonly \
  --mount type=volume,src=mini-pgbench-results,dst=/data \
  mini_pgbench
```

The scratch runtime contains the pure-Go binary and CA roots, and runs as UID/GID
65532. TLS files must be readable by that user; bind-mounted data directories must
be writable by it. `localhost` in a container is the container, not your database
host. Omit the TLS mount only if your configuration does not reference TLS files.

After this implementation is reviewed and merged, the main workflow publishes
`ghcr.io/djosh34/mini_pgbench:latest` and `:<full-commit-SHA>`. Each tag is one
multi-platform image index for **linux/amd64 and linux/arm64**; there are no
architecture-specific user tags. Publication is not performed by PR CI. GHCR
package visibility/access must allow your Docker/Kubernetes environment to pull it.

## Kubernetes

Edit hosts/database/user in [`deploy/benchmark.yaml`](deploy/benchmark.yaml), then:

```sh
kubectl create secret generic mini-pgbench-tls \
  --from-file=ca.crt=tls/ca.crt \
  --from-file=client.crt=tls/client.crt \
  --from-file=client.key=tls/client.key
kubectl apply -f deploy/benchmark.yaml
kubectl port-forward pod/mini-pgbench 8080:8080
```

This is only a Pod and ConfigMap, with a **pre-created** TLS Secret and `emptyDir`.
The sample uses certificate authentication and contains no real credentials. If a
password is necessary, do not commit it in the manifest; mount a privately prepared
configuration from a Secret instead. Secret and configuration changes are not
reloaded: recreate the Pod. `emptyDir` survives container restarts, but application
startup still clears history. No Service, Deployment, PVC, Helm chart or operator
is required.

## Prometheus

Scrape `GET /metrics`. Counters survive start/stop and reset with the process.
There are no percentile gauges or per-row/per-host labels.

```promql
# Successful TPS and failed transactions/second
rate(mini_pgbench_transactions_total[1m])
rate(mini_pgbench_errors_total[1m])

# Successful p95 latency in seconds
histogram_quantile(0.95, sum by (le) (rate(mini_pgbench_transaction_duration_seconds_bucket[1m])))

# Running state and database/sql pool utilization
mini_pgbench_running
go_sql_in_use_connections{db_name="postgres"}
go_sql_max_open_connections{db_name="postgres"}
```

The official Prometheus histogram covers 50 microseconds through 60 seconds plus
`+Inf`. The official SQL pool collector also exports idle connections, connection
wait counts/durations, and connections closed by pool limits.

## Tests, builds and vulnerability gate

```sh
go test ./...
go vet ./...
go test -race ./...
# Requires Docker and OpenSSL; generates disposable fixture certificates.
tests/integration.sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/mini_pgbench-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/mini_pgbench-arm64 .

# Repeat for linux/arm64.
docker buildx build --platform linux/amd64 --output type=docker,dest=image.tar .
docker run --rm -v "$PWD:/work:ro" aquasec/trivy:0.76.0 image \
  --input /work/image.tar --scanners vuln --severity HIGH,CRITICAL \
  --exit-code 1 --ignorefile /dev/null
```

Without fixture variables, ordinary tests explicitly skip PostgreSQL tests. The
integration script runs PostgreSQL 18.6 with generated CA/server/client certificates
and a benchmark role with **NOCREATEDB**, tests standard TLS modes, automatic
initialization, transactions, pool acquisition cancellation, repeated lifecycle
changes, process shutdown and fatal SQLite-write behavior. Only the fixture's
administrator creates its disposable test database.

PR and main CI run tests, race tests, vet, pure-Go builds, mTLS integration and image
scans on both native architectures. Main publication depends on both passing.
Trivy rejects every HIGH/CRITICAL finding, **including unfixed** findings, with no
suppression file. Remediate actual dependency/runtime findings rather than bypassing
the gate. Clean scans describe the database at scan time, not a future guarantee.

Go and direct dependencies were resolved from their stable upstream releases at
implementation time and are pinned in `go.mod`/`go.sum`. Keep the project small:
additional production frameworks, speculative hardening machinery, custom integrity
gates, host orchestration and features outside this test-tool scope are not goals.
