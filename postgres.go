package main

import (
	"context"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

func openPostgres(ctx context.Context, c Config) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(c.dsn())
	if err != nil {
		return nil, err
	}
	config.MaxConns = int32(min(c.Pool.MaxOpen, math.MaxInt32))
	// pgxpool has no limit on idle connections, so pool.max_idle has no effect.
	// pgxpool reads a zero MaxConnLifetime as no limit, but a zero
	// MaxConnIdleTime closes every idle connection.
	config.MaxConnLifetime = c.Pool.MaxLifetime
	config.MaxConnIdleTime = c.Pool.MaxIdleTime
	if config.MaxConnIdleTime == 0 {
		config.MaxConnIdleTime = time.Duration(math.MaxInt64)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func initialize(ctx context.Context, pool *pgxpool.Pool, rows int64) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS mini_pgbench_accounts (
			id bigint PRIMARY KEY,
			balance bigint NOT NULL DEFAULT 0
		)`)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO mini_pgbench_accounts (id, balance)
		SELECT generate_series(1, $1::bigint), 0
		ON CONFLICT (id) DO NOTHING`, rows)
	return err
}

// transaction sends BEGIN, the statement and COMMIT as one batch, so a
// transaction costs one network round trip. If the statement fails,
// PostgreSQL skips the COMMIT and the pool closes the connection, because it
// is still inside the failed transaction.
func transaction(ctx context.Context, pool *pgxpool.Pool, write bool, id int64) error {
	var b pgx.Batch
	b.Queue("BEGIN")
	if write {
		b.Queue(`UPDATE mini_pgbench_accounts SET balance = balance + 1 WHERE id = $1`, id)
	} else {
		var balance int64
		b.Queue(`SELECT balance FROM mini_pgbench_accounts WHERE id = $1`, id).QueryRow(func(row pgx.Row) error {
			return row.Scan(&balance)
		})
	}
	b.Queue("COMMIT")
	return pool.SendBatch(ctx, &b).Close()
}

// poolCollector exports the pgxpool statistics to Prometheus.
type poolCollector struct{ pool *pgxpool.Pool }

func poolDesc(name, help string) *prometheus.Desc {
	return prometheus.NewDesc("mini_pgbench_pool_"+name, help, nil, nil)
}

var (
	poolMaxConns     = poolDesc("max_connections", "Maximum number of connections, pool.max_open.")
	poolConns        = poolDesc("connections", "Open connections, in use or idle.")
	poolInUse        = poolDesc("in_use_connections", "Connections in use by a transaction.")
	poolIdle         = poolDesc("idle_connections", "Idle connections.")
	poolAcquires     = poolDesc("acquires_total", "Successful connection acquires.")
	poolWaits        = poolDesc("waits_total", "Successful acquires that waited because no connection was idle.")
	poolWaitSeconds  = poolDesc("wait_seconds_total", "Time those acquires waited.")
	poolCanceled     = poolDesc("canceled_acquires_total", "Acquires cancelled by their context while waiting.")
	poolNewConns     = poolDesc("new_connections_total", "Connections opened.")
	poolLifetimeDone = poolDesc("max_lifetime_closed_total", "Connections closed because of pool.max_lifetime.")
	poolIdleDone     = poolDesc("max_idle_time_closed_total", "Connections closed because of pool.max_idle_time.")
)

func (c poolCollector) Describe(ch chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(c, ch)
}

func (c poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	gauge := func(d *prometheus.Desc, v int32) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, float64(v))
	}
	counter := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v)
	}
	gauge(poolMaxConns, s.MaxConns())
	gauge(poolConns, s.TotalConns())
	gauge(poolInUse, s.AcquiredConns())
	gauge(poolIdle, s.IdleConns())
	counter(poolAcquires, float64(s.AcquireCount()))
	counter(poolWaits, float64(s.EmptyAcquireCount()))
	counter(poolWaitSeconds, s.EmptyAcquireWaitTime().Seconds())
	counter(poolCanceled, float64(s.CanceledAcquireCount()))
	counter(poolNewConns, float64(s.NewConnsCount()))
	counter(poolLifetimeDone, float64(s.MaxLifetimeDestroyCount()))
	counter(poolIdleDone, float64(s.MaxIdleDestroyCount()))
}
