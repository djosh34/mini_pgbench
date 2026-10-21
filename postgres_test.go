package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pgTestConfig points at the mTLS PostgreSQL that tests/integration.sh starts.
func pgTestConfig(t *testing.T) Config {
	t.Helper()
	dir := os.Getenv("TEST_PG_TLS")
	if dir == "" {
		t.Skip("run tests/integration.sh for a disposable mTLS PostgreSQL")
	}
	port, err := strconv.Atoi(os.Getenv("TEST_PG_PORT"))
	if err != nil {
		t.Fatal(err)
	}
	c := testConfig()
	c.Postgres.Hosts = []string{"localhost"}
	c.Postgres.Port = port
	c.Postgres.Database = "benchmark"
	c.Postgres.User = "benchmark"
	c.Postgres.SSLMode = "verify-full"
	c.Postgres.SSLRootCert = filepath.Join(dir, "ca.crt")
	c.Postgres.SSLCert = filepath.Join(dir, "client.crt")
	c.Postgres.SSLKey = filepath.Join(dir, "client.key")
	c.Pool.MaxOpen = 2
	c.Pool.MaxIdle = 1
	return c
}

func TestPostgres(t *testing.T) {
	c := pgTestConfig(t)
	c.Postgres.Hosts = []string{"localhost", "127.0.0.1"}
	c.Pool.MaxLifetime = time.Minute
	c.Pool.MaxIdleTime = time.Second
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, err := openPostgres(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var createdb bool
	if err = pool.QueryRow(ctx, "SELECT rolcreatedb FROM pg_roles WHERE rolname=current_user").Scan(&createdb); err != nil || createdb {
		t.Fatalf("benchmark role CREATEDB=%v, %v", createdb, err)
	}
	if err = initialize(ctx, pool, c.Rows); err != nil {
		t.Fatal(err)
	}
	if err = transaction(ctx, pool, false, 1); err != nil {
		t.Fatal(err)
	}
	if err = transaction(ctx, pool, true, 1); err != nil {
		t.Fatal(err)
	}
	if err = initialize(ctx, pool, c.Rows+2); err != nil {
		t.Fatal(err)
	}
	var balance int64
	if err = pool.QueryRow(ctx, "SELECT balance FROM mini_pgbench_accounts WHERE id=1").Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 1 {
		t.Fatal("initialization reset data")
	}
	var count int64
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM mini_pgbench_accounts").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != c.Rows+2 {
		t.Fatal("missing seed rows")
	}
	if pool.Stat().MaxConns() != 2 {
		t.Fatal("pool max open")
	}
	ps, err := newStats(filepath.Join(t.TempDir(), "results.db"), time.Now(), pool)
	if err != nil {
		t.Fatal(err)
	}
	defer ps.close()
	w := httptest.NewRecorder()
	(&App{stats: ps}).handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), "\nmini_pgbench_pool_max_connections 2\n") {
		t.Fatalf("pool metrics missing: %s", w.Body.String())
	}
	// Reading a missing row is an error.
	if err = transaction(ctx, pool, false, c.Rows+3); err == nil {
		t.Fatal("missing row not reported")
	}
	// Cancelling the context must interrupt a query that is blocked inside PostgreSQL.
	short, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	start := time.Now()
	if _, err = pool.Exec(short, "SELECT pg_sleep(10)"); err == nil || time.Since(start) > time.Second {
		t.Fatalf("query cancellation: %v", err)
	}
	// The timeout also covers the wait for the only pool connection.
	one := c
	one.Pool.MaxOpen = 1
	single, err := openPostgres(ctx, one)
	if err != nil {
		t.Fatal(err)
	}
	held, err := single.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	short2, stop2 := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop2()
	if err = transaction(short2, single, false, 1); err == nil {
		t.Fatal("pool acquisition not cancelled")
	}
	held.Release()
	single.Close()
	s := testStats(t)
	a := &App{config: c, stats: s, op: func(ctx context.Context, w bool, id int64) error {
		return transaction(ctx, pool, w, id)
	}}
	for range 3 {
		a.start()
		time.Sleep(30 * time.Millisecond)
		a.stop()
	}
	a.close()
	if _, err = pool.Exec(ctx, "DROP TABLE mini_pgbench_accounts"); err != nil {
		t.Fatal(err)
	}
	if err = transaction(ctx, pool, false, 1); err == nil {
		t.Fatal("missing table error swallowed")
	}
	for _, mode := range []string{"allow", "prefer", "require", "verify-ca", "verify-full"} {
		t.Run(mode, func(t *testing.T) {
			mc := c
			mc.Postgres.SSLMode = mode
			p, e := openPostgres(ctx, mc)
			if e != nil {
				t.Fatal(e)
			}
			p.Close()
		})
	}
	c.Postgres.SSLCert = ""
	c.Postgres.SSLKey = ""
	if p, e := openPostgres(ctx, c); e == nil {
		p.Close()
		t.Fatal("mTLS accepted without client certificate")
	}
	c.Postgres.SSLMode = "disable"
	if p, e := openPostgres(ctx, c); e == nil {
		p.Close()
		t.Fatal("fixture unexpectedly accepts plaintext")
	}
}
