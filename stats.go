package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	_ "modernc.org/sqlite"
)

type Summary struct {
	Timestamp         time.Time `json:"timestamp"`
	TPS               int64     `json:"tps"`
	Errors            int64     `json:"errors"`
	P50               *float64  `json:"p50_ms"`
	P95               *float64  `json:"p95_ms"`
	P99               *float64  `json:"p99_ms"`
	TotalTransactions int64     `json:"total_transactions"`
	TotalErrors       int64     `json:"total_errors"`
}

type window struct {
	successes, errors int64
	histogram         *hdr.Histogram
}

type Stats struct {
	mu      sync.Mutex
	epoch   time.Time
	next    int64
	windows map[int64]*window

	// Only flush touches these.
	totalTransactions, totalErrors int64

	history           *sql.DB // writer, one connection
	reader            *sql.DB // for /results, so a slow download never blocks the writer
	registry          *prometheus.Registry
	successes, errors prometheus.Counter
	latency           prometheus.Histogram
	running           prometheus.Gauge
}

const schema = `
DROP TABLE IF EXISTS summaries;
CREATE TABLE summaries (
	second INTEGER PRIMARY KEY,
	timestamp TEXT NOT NULL,
	tps INTEGER NOT NULL,
	errors INTEGER NOT NULL,
	p50 REAL,
	p95 REAL,
	p99 REAL,
	total_transactions INTEGER NOT NULL,
	total_errors INTEGER NOT NULL
)`

func newStats(path string, epoch time.Time, pool *pgxpool.Pool) (*Stats, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// Start with an empty history, also after a container restart that keeps the emptyDir.
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	reader, err := sql.Open("sqlite", dsn)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Stats{
		epoch:    epoch,
		windows:  make(map[int64]*window),
		history:  db,
		reader:   reader,
		registry: prometheus.NewRegistry(),
	}
	s.successes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mini_pgbench_transactions_total",
		Help: "Committed transactions.",
	})
	s.errors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mini_pgbench_errors_total",
		Help: "Failed transactions. Transactions cancelled by a stop are not counted.",
	})
	s.latency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "mini_pgbench_transaction_duration_seconds",
		Help: "Latency of committed transactions, including waiting for a pool connection.",
		Buckets: []float64{
			.00005, .0001, .00025, .0005, .001, .0025, .005, .01, .025, .05,
			.1, .25, .5, 1, 2.5, 5, 10, 30, 60,
		},
	})
	s.running = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mini_pgbench_running",
		Help: "1 while the workload runs, 0 while it is stopped.",
	})
	s.registry.MustRegister(s.successes, s.errors, s.latency, s.running)
	if pool != nil {
		s.registry.MustRegister(poolCollector{pool})
	}
	return s, nil
}

func (s *Stats) close() error {
	return errors.Join(s.reader.Close(), s.history.Close())
}

func (s *Stats) record(elapsed time.Duration, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	second := int64(time.Since(s.epoch) / time.Second)
	w := s.windows[second]
	if w == nil {
		w = &window{histogram: hdr.New(1, math.MaxInt64/1000, 3)}
		s.windows[second] = w
	}
	if failed {
		w.errors++
		s.errors.Inc()
		return
	}
	w.successes++
	s.successes.Inc()
	s.latency.Observe(elapsed.Seconds())
	_ = w.histogram.RecordValue(max(elapsed.Microseconds(), 1))
}

// flush writes one row for every finished second up to now. Only the
// collector goroutine calls it. It copies the finished windows out under mu,
// then writes them to SQLite without holding the lock.
func (s *Stats) flush(now time.Time) error {
	s.mu.Lock()
	end := int64(now.Sub(s.epoch) / time.Second)
	var summaries []Summary
	first := s.next
	for ; s.next < end; s.next++ {
		v := Summary{Timestamp: s.epoch.Add(time.Duration(s.next) * time.Second).UTC()}
		if w := s.windows[s.next]; w != nil {
			v.TPS = w.successes
			v.Errors = w.errors
			if w.successes > 0 {
				v.P50 = new(float64(w.histogram.ValueAtQuantile(50)) / 1000)
				v.P95 = new(float64(w.histogram.ValueAtQuantile(95)) / 1000)
				v.P99 = new(float64(w.histogram.ValueAtQuantile(99)) / 1000)
			}
		}
		summaries = append(summaries, v)
		delete(s.windows, s.next)
	}
	s.mu.Unlock()
	for i, v := range summaries {
		s.totalTransactions += v.TPS
		s.totalErrors += v.Errors
		_, err := s.history.Exec(
			`INSERT INTO summaries (second, timestamp, tps, errors, p50, p95, p99, total_transactions, total_errors)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			first+int64(i), v.Timestamp.Format(time.RFC3339Nano), v.TPS, v.Errors, v.P50, v.P95, v.P99,
			s.totalTransactions, s.totalErrors,
		)
		if err != nil {
			return fmt.Errorf("write SQLite summary: %w", err)
		}
	}
	return nil
}

func (s *Stats) collect(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.flush(time.Now()); err != nil {
				return err
			}
		}
	}
}

// results writes the history as a JSON array, oldest first. A positive limit
// keeps only the latest limit rows. There is one row per second, so that is a
// primary key range and needs no sort.
func (s *Stats) results(ctx context.Context, out io.Writer, limit int) error {
	const columns = `SELECT timestamp, tps, errors, p50, p95, p99, total_transactions, total_errors FROM summaries`
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = s.reader.QueryContext(ctx,
			columns+` WHERE second > (SELECT max(second) FROM summaries) - ? ORDER BY second`, limit)
	} else {
		rows, err = s.reader.QueryContext(ctx, columns+` ORDER BY second`)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	if _, err = io.WriteString(out, "["); err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	first := true
	for rows.Next() {
		var v Summary
		var stamp string
		err = rows.Scan(&stamp, &v.TPS, &v.Errors, &v.P50, &v.P95, &v.P99, &v.TotalTransactions, &v.TotalErrors)
		if err != nil {
			return err
		}
		if v.Timestamp, err = time.Parse(time.RFC3339Nano, stamp); err != nil {
			return err
		}
		if !first {
			if _, err = io.WriteString(out, ","); err != nil {
				return err
			}
		}
		first = false
		if err = enc.Encode(v); err != nil {
			return err
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	_, err = io.WriteString(out, "]\n")
	return err
}
