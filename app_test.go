package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5"
)

func testStats(t *testing.T) *Stats {
	t.Helper()
	return testStatsAt(t, filepath.Join(t.TempDir(), "results.db"))
}

func testStatsAt(t *testing.T, path string) *Stats {
	t.Helper()
	s, err := newStats(path, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.close() })
	return s
}

func testConfig() Config {
	return Config{Workers: 3, Rows: 10, ReadWeight: 1}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	waitForWithin(t, 2*time.Second, f)
}

func waitForWithin(t *testing.T, timeout time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func queryInt(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestConfig(t *testing.T) {
	const base = `postgres:
  hosts: [localhost, replica]
  database: bench
  user: bench
  sslmode: disable
`
	for _, tc := range []struct {
		name, extra string
		valid       bool
	}{
		{"defaults", "", true},
		{"timeout", "transaction_timeout: 3s\n", true},
		{"negative workers", "workers: -1\n", false},
		{"zero rows", "rows: 0\n", false},
		{"weights", "read_weight: 0\nwrite_weight: 0\n", false},
		{"negative weight", "write_weight: -1\n", false},
		{"duration", "transaction_timeout: -1s\n", false},
		{"bad duration", "transaction_timeout: xyz\n", false},
		{"unknown", "typo: 1\n", false},
		{"pool", "pool:\n  max_open: 1\n  max_idle: 2\n", false},
		{"documents", "---\nworkers: 2\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(base+tc.extra), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := loadConfig(p)
			if (err == nil) != tc.valid {
				t.Fatalf("config error=%v", err)
			}
			if tc.valid && c.Rows != 100000 {
				t.Fatal("row default")
			}
		})
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	missingCA := strings.Replace(base, "sslmode: disable", "sslmode: verify-full\n  sslrootcert: /nonexistent", 1)
	if err := os.WriteFile(p, []byte(missingCA), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(p); err == nil {
		t.Fatal("missing certificate accepted")
	}
}

func TestYAMLOverridesAmbientPostgresSettings(t *testing.T) {
	for key, value := range map[string]string{
		"PGSERVICE": "nonexistent", "PGSERVICEFILE": "/nonexistent/service.conf",
		"PGOPTIONS": "-c default_transaction_read_only=on", "PGTARGETSESSIONATTRS": "read-only",
		"PGHOST": "wrong-host", "PGDATABASE": "wrong-database", "PGSSLMODE": "not-a-mode",
	} {
		t.Setenv(key, value)
	}
	clearPostgresEnv()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("postgres:\n  hosts: [yaml-host]\n  database: yaml-database\n  user: yaml-user\n  sslmode: disable\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pgx.ParseConfig(c.dsn())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "yaml-host" || parsed.Database != "yaml-database" || parsed.User != "yaml-user" ||
		parsed.TLSConfig != nil || parsed.ValidateConnect != nil || parsed.RuntimeParams["options"] != "" {
		t.Fatalf("ambient PostgreSQL settings affected YAML: %+v", parsed.Config)
	}
}

func TestWeights(t *testing.T) {
	if chooseWrite(1, 0) || !chooseWrite(0, 1) {
		t.Fatal("weight edge")
	}
	writes := 0
	for range 10000 {
		if chooseWrite(1, 3) {
			writes++
		}
	}
	if writes < 7000 || writes > 8000 {
		t.Fatal(writes)
	}
}

func TestLifecycle(t *testing.T) {
	s := testStats(t)
	var active, peak atomic.Int64
	a := &App{config: testConfig(), stats: s, op: func(ctx context.Context, _ bool, _ int64) error {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer active.Add(-1)
		<-ctx.Done()
		return ctx.Err()
	}}
	for range 5 {
		a.start()
		a.start()
		waitFor(t, func() bool { return active.Load() == 3 })
		a.stop()
		a.stop()
		if active.Load() != 0 {
			t.Fatal("workers not joined")
		}
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			a.start()
			a.stop()
		})
	}
	wg.Wait()
	a.close()
	if a.start() || peak.Load() > 3 {
		t.Fatal("overlapping workers")
	}
	if err := s.flush(s.epoch.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if queryInt(t, s.history, "SELECT errors FROM summaries") != 0 {
		t.Fatal("stop counted as failure")
	}
}

func TestTimeoutAndErrorBackoff(t *testing.T) {
	for _, timeout := range []time.Duration{0, 5 * time.Millisecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := testStats(t)
				c := testConfig()
				c.Workers = 1
				c.TransactionTimeout = timeout
				var calls atomic.Int64
				a := &App{config: c, stats: s, op: func(ctx context.Context, _ bool, _ int64) error {
					calls.Add(1)
					if timeout > 0 {
						<-ctx.Done()
						return ctx.Err()
					}
					return errors.New("database failure")
				}}
				a.start()
				time.Sleep(40 * time.Millisecond)
				a.stop()
				if calls.Load() != 1 {
					t.Fatalf("calls=%d, want 1: failure retried without backoff", calls.Load())
				}
				if err := s.flush(s.epoch.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if n := queryInt(t, s.history, "SELECT errors FROM summaries"); n != 1 {
					t.Fatalf("failures=%d", n)
				}
			})
		})
	}
}

func getResults(t *testing.T, h http.Handler, query string) []Summary {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/results"+query, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", query, w.Code, w.Body.String())
	}
	var rows []Summary
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestResults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := testStats(t)
		a := &App{config: testConfig(), stats: s}
		h := a.handler()
		// Second 0: two commits and an error. Second 1: idle. Second 2: an error. Second 3: a commit.
		s.record(time.Millisecond, false)
		s.record(3*time.Millisecond, false)
		s.record(0, true)
		time.Sleep(2 * time.Second)
		s.record(0, true)
		time.Sleep(time.Second)
		s.record(time.Millisecond, false)
		if err := s.flush(s.epoch.Add(4 * time.Second)); err != nil {
			t.Fatal(err)
		}

		all := getResults(t, h, "")
		want := []struct{ tps, errors, totalTx, totalErr int64 }{
			{2, 1, 2, 1},
			{0, 0, 2, 1},
			{0, 1, 2, 2},
			{1, 0, 3, 2},
		}
		if len(all) != len(want) {
			t.Fatalf("got %d rows, want %d", len(all), len(want))
		}
		for i, w := range want {
			r := all[i]
			if r.TPS != w.tps || r.Errors != w.errors || r.TotalTransactions != w.totalTx || r.TotalErrors != w.totalErr {
				t.Fatalf("row %d: %+v, want %+v", i, r, w)
			}
			if !r.Timestamp.Equal(s.epoch.Add(time.Duration(i) * time.Second)) {
				t.Fatalf("row %d timestamp %v", i, r.Timestamp)
			}
		}
		if all[0].P50 == nil || all[1].P50 != nil || all[2].P50 != nil {
			t.Fatal("percentiles must be null in seconds without commits")
		}

		latest := getResults(t, h, "?limit=2")
		if len(latest) != 2 || !latest[0].Timestamp.Equal(all[2].Timestamp) ||
			!latest[1].Timestamp.Equal(all[3].Timestamp) || latest[1].TotalTransactions != 3 {
			t.Fatalf("limit=2: %+v", latest)
		}
		if got := getResults(t, h, "?limit=100"); len(got) != 4 {
			t.Fatalf("limit=100 returned %d rows", len(got))
		}
	})
}

func TestResultsReadDoesNotBlockWriter(t *testing.T) {
	s := testStats(t)
	if err := s.flush(s.epoch.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := s.reader.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode=%q, %v", mode, err)
	}
	rows, err := s.reader.Query("SELECT second FROM summaries")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no rows")
	}
	start := time.Now()
	if err := s.flush(s.epoch.Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("flush waited for the open read")
	}
}

func TestHistoryResetsOnStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.db")
	s, err := newStats(path, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.flush(s.epoch.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	fresh := testStatsAt(t, path)
	if queryInt(t, fresh.history, "SELECT count(*) FROM summaries") != 0 {
		t.Fatal("history retained")
	}
}

func TestStorageFailure(t *testing.T) {
	s := testStats(t)
	if _, err := s.history.Exec("DROP TABLE summaries"); err != nil {
		t.Fatal(err)
	}
	if err := s.flush(s.epoch.Add(time.Second)); err == nil {
		t.Fatal("write failure not returned")
	}
}

func TestHTTP(t *testing.T) {
	s := testStats(t)
	a := &App{config: testConfig(), stats: s, op: func(ctx context.Context, _ bool, _ int64) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	defer a.close()
	const badLimit = `{"error":"limit must be a positive integer"}`
	h := a.handler()
	for _, tc := range []struct {
		method, path string
		code         int
		contains     string
		json         bool
	}{
		{"GET", "/status", 200, `"running":false`, true},
		{"HEAD", "/status", 200, "", true},
		{"GET", "/start", 405, "Method Not Allowed", false},
		{"POST", "/start", 200, `"running":true`, true},
		{"GET", "/status", 200, `"running":true`, true},
		{"POST", "/stop", 200, `"running":false`, true},
		{"GET", "/results", 200, "[]", true},
		{"GET", "/results?limit=5", 200, "[]", true},
		{"GET", "/results?limit=", 400, badLimit, true},
		{"GET", "/results?limit=0", 400, badLimit, true},
		{"GET", "/results?limit=-1", 400, badLimit, true},
		{"GET", "/results?limit=abc", 400, badLimit, true},
		{"GET", "/results?limit=1.5", 400, badLimit, true},
		{"GET", "/metrics", 200, "mini_pgbench_transaction_duration_seconds_bucket", false},
		{"GET", "/missing", 404, "404 page not found", false},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.contains) {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		if tc.json && !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s %s: not JSON", tc.method, tc.path)
		}
	}
}
