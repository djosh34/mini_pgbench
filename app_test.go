package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testStats(t *testing.T) *Stats {
	t.Helper()
	s, err := newStats(filepath.Join(t.TempDir(), "results.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.history.Close() })
	return s
}
func testConfig() Config { var c Config; c.Workers = 3; c.Rows = 10; c.ReadWeight = 1; return c }
func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestConfig(t *testing.T) {
	base := "postgres:\n  hosts: [localhost, replica]\n  database: bench\n  user: bench\n  sslmode: disable\n"
	for _, tc := range []struct {
		name, extra string
		valid       bool
	}{
		{"defaults", "", true}, {"timeout", "transaction_timeout: 3s\n", true}, {"negative workers", "workers: -1\n", false}, {"zero rows", "rows: 0\n", false}, {"weights", "read_weight: 0\nwrite_weight: 0\n", false}, {"negative weight", "write_weight: -1\n", false}, {"duration", "transaction_timeout: -1s\n", false}, {"bad duration", "transaction_timeout: xyz\n", false}, {"unknown", "typo: 1\n", false}, {"pool", "pool:\n  max_open: 1\n  max_idle: 2\n", false}, {"documents", "---\nworkers: 2\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			os.WriteFile(p, []byte(base+tc.extra), 0600)
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
	os.WriteFile(p, []byte(strings.Replace(base, "sslmode: disable", "sslmode: verify-full\n  sslrootcert: /nonexistent", 1)), 0600)
	if _, err := loadConfig(p); err == nil {
		t.Fatal("missing certificate accepted")
	}
}

func TestWeights(t *testing.T) {
	for i := 0; i < 100; i++ {
		if chooseWrite(1, 0) || !chooseWrite(0, 1) {
			t.Fatal("weight edge")
		}
	}
	writes := 0
	for i := 0; i < 10000; i++ {
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
	for i := 0; i < 5; i++ {
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
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a.start(); a.stop() }()
	}
	wg.Wait()
	a.close()
	if a.start() || peak.Load() > 3 {
		t.Fatal("overlapping workers")
	}
	if err := s.flush(s.epoch.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var failures int
	s.history.QueryRow("SELECT errors FROM summaries").Scan(&failures)
	if failures != 0 {
		t.Fatal("stop counted as failure")
	}
}

func TestTimeoutAndErrorBackoff(t *testing.T) {
	for _, timeout := range []time.Duration{0, 5 * time.Millisecond} {
		t.Run(timeout.String(), func(t *testing.T) {
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
			waitFor(t, func() bool { return calls.Load() > 0 })
			time.Sleep(40 * time.Millisecond)
			a.stop()
			if calls.Load() != 1 {
				t.Fatal("failure retried without backoff")
			}
			s.flush(s.epoch.Add(time.Second))
			var failures int
			s.history.QueryRow("SELECT errors FROM summaries").Scan(&failures)
			if failures != 1 {
				t.Fatalf("failures=%d", failures)
			}
		})
	}
}

func TestHistoryAndMetrics(t *testing.T) {
	s := testStats(t)
	s.record(time.Millisecond, false)
	s.record(3*time.Millisecond, false)
	s.record(0, true)
	if err := s.flush(s.epoch.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	out := httptest.NewRecorder()
	if err := s.results(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	body := out.Body.String()
	if !strings.Contains(body, `"tps":2`) || !strings.Contains(body, `"errors":1`) || strings.Count(body, `"p50_ms":null`) != 2 {
		t.Fatal(body)
	}
	// A new process resets history even at the same path.
	var path string
	s.history.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path)
	s.history.Close()
	fresh, err := newStats(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.history.Close()
	var count int
	fresh.history.QueryRow("SELECT count(*) FROM summaries").Scan(&count)
	if count != 0 {
		t.Fatal("history retained")
	}
}

func TestStorageFailure(t *testing.T) {
	s := testStats(t)
	s.history.Exec("DROP TABLE summaries")
	if err := s.flush(s.epoch.Add(time.Second)); err == nil {
		t.Fatal("write failure not returned")
	}
}

func TestHTTP(t *testing.T) {
	s := testStats(t)
	a := &App{config: testConfig(), stats: s, op: func(ctx context.Context, _ bool, _ int64) error { <-ctx.Done(); return ctx.Err() }}
	defer a.close()
	for _, tc := range []struct {
		method, path string
		code         int
		contains     string
	}{
		{"GET", "/status", 200, `"running":false`}, {"GET", "/start", 405, `"error"`}, {"POST", "/start", 200, `"running":true`}, {"POST", "/start", 200, `"running":true`}, {"POST", "/stop", 200, `"running":false`}, {"GET", "/results", 200, "[]"}, {"GET", "/metrics", 200, "mini_pgbench_transaction_duration_seconds_bucket"}, {"GET", "/missing", 404, `"error"`},
	} {
		w := httptest.NewRecorder()
		a.handler().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.contains) {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		if tc.path != "/metrics" && !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
			t.Fatal("not JSON")
		}
	}
}
