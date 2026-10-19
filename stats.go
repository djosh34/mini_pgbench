package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	_ "modernc.org/sqlite"
)

type Summary struct {
	Timestamp time.Time `json:"timestamp"`
	TPS       int64     `json:"tps"`
	Errors    int64     `json:"errors"`
	P50       *float64  `json:"p50_ms"`
	P95       *float64  `json:"p95_ms"`
	P99       *float64  `json:"p99_ms"`
}

type window struct {
	successes, errors int64
	histogram         *hdr.Histogram
}
type Stats struct {
	mu                sync.Mutex
	epoch             time.Time
	next              int64
	windows           map[int64]*window
	history           *sql.DB
	registry          *prometheus.Registry
	successes, errors prometheus.Counter
	latency           prometheus.Histogram
	running           prometheus.Gauge
}

func newStats(path string, pool *sql.DB) (*Stats, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// Reset in place, including after a container restart with the same emptyDir.
	_, err = db.Exec(`DROP TABLE IF EXISTS summaries; CREATE TABLE summaries (second INTEGER PRIMARY KEY, timestamp TEXT NOT NULL, tps INTEGER NOT NULL, errors INTEGER NOT NULL, p50 REAL, p95 REAL, p99 REAL)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Stats{epoch: time.Now(), windows: make(map[int64]*window), history: db, registry: prometheus.NewRegistry()}
	s.successes = prometheus.NewCounter(prometheus.CounterOpts{Name: "mini_pgbench_transactions_total", Help: "Successful committed transactions."})
	s.errors = prometheus.NewCounter(prometheus.CounterOpts{Name: "mini_pgbench_errors_total", Help: "Failed transactions, excluding explicit stop cancellation."})
	s.latency = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "mini_pgbench_transaction_duration_seconds", Help: "Successful transaction latency including pool acquisition and commit.", Buckets: []float64{.00005, .0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}})
	s.running = prometheus.NewGauge(prometheus.GaugeOpts{Name: "mini_pgbench_running", Help: "Whether workload workers are running."})
	s.registry.MustRegister(s.successes, s.errors, s.latency, s.running)
	if pool != nil {
		s.registry.MustRegister(collectors.NewDBStatsCollector(pool, "postgres"))
	}
	return s, nil
}

func (s *Stats) record(elapsed time.Duration, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	second := int64(time.Since(s.epoch) / time.Second)
	w := s.windows[second]
	if w == nil {
		w = &window{histogram: hdr.New(1, int64(^uint64(0)>>1)/1000, 3)}
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
	micros := elapsed.Microseconds()
	if micros < 1 {
		micros = 1
	}
	_ = w.histogram.RecordValue(micros)
}

func (s *Stats) flush(now time.Time) error {
	// The writer is unique. Detach completed windows while holding the outcome lock.
	s.mu.Lock()
	end := int64(now.Sub(s.epoch) / time.Second)
	summaries := make([]Summary, 0)
	first := s.next
	for s.next < end {
		i := s.next
		w := s.windows[i]
		v := Summary{Timestamp: s.epoch.Add(time.Duration(i) * time.Second).UTC()}
		if w != nil {
			v.TPS = w.successes
			v.Errors = w.errors
			if w.successes > 0 {
				a, b, c := float64(w.histogram.ValueAtQuantile(50))/1000, float64(w.histogram.ValueAtQuantile(95))/1000, float64(w.histogram.ValueAtQuantile(99))/1000
				v.P50 = &a
				v.P95 = &b
				v.P99 = &c
			}
		}
		summaries = append(summaries, v)
		delete(s.windows, i)
		s.next++
	}
	s.mu.Unlock()
	for i, v := range summaries {
		if _, err := s.history.Exec(`INSERT INTO summaries VALUES (?, ?, ?, ?, ?, ?, ?)`, first+int64(i), v.Timestamp.Format(time.RFC3339Nano), v.TPS, v.Errors, v.P50, v.P95, v.P99); err != nil {
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

func (s *Stats) results(ctx context.Context, out io.Writer) error {
	rows, err := s.history.QueryContext(ctx, `SELECT timestamp,tps,errors,p50,p95,p99 FROM summaries ORDER BY second`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if _, err = io.WriteString(out, "["); err != nil {
		return err
	}
	first := true
	for rows.Next() {
		var v Summary
		var stamp string
		if err = rows.Scan(&stamp, &v.TPS, &v.Errors, &v.P50, &v.P95, &v.P99); err != nil {
			return err
		}
		v.Timestamp, err = time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return err
		}
		if !first {
			if _, err = io.WriteString(out, ","); err != nil {
				return err
			}
		}
		first = false
		if err = json.NewEncoder(out).Encode(v); err != nil {
			return err
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	_, err = io.WriteString(out, "]\n")
	return err
}
