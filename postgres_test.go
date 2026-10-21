package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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
	db, pool, err := openPostgres(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var createdb bool
	if err = db.Raw("SELECT rolcreatedb FROM pg_roles WHERE rolname=current_user").Scan(&createdb).Error; err != nil || createdb {
		t.Fatalf("benchmark role CREATEDB=%v, %v", createdb, err)
	}
	if err = initialize(ctx, db, c.Rows); err != nil {
		t.Fatal(err)
	}
	if err = transaction(ctx, db, false, 1); err != nil {
		t.Fatal(err)
	}
	if err = transaction(ctx, db, true, 1); err != nil {
		t.Fatal(err)
	}
	if err = initialize(ctx, db, c.Rows+2); err != nil {
		t.Fatal(err)
	}
	var balance int64
	if err = db.Raw("SELECT balance FROM mini_pgbench_accounts WHERE id=1").Scan(&balance).Error; err != nil {
		t.Fatal(err)
	}
	if balance != 1 {
		t.Fatal("initialization reset data")
	}
	var count int64
	if err = db.Raw("SELECT count(*) FROM mini_pgbench_accounts").Scan(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != c.Rows+2 {
		t.Fatal("missing seed rows")
	}
	if pool.Stats().MaxOpenConnections != 2 {
		t.Fatal("pool max open")
	}
	// Cancellation must interrupt a transaction blocked in PostgreSQL, not just a fake operation.
	short, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	start := time.Now()
	if err = db.WithContext(short).Exec("SELECT pg_sleep(10)").Error; err == nil || time.Since(start) > time.Second {
		t.Fatalf("query cancellation: %v", err)
	}
	// Timeout includes waiting to acquire the only available connection.
	pool.SetMaxOpenConns(1)
	held, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	short2, stop2 := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop2()
	if err = transaction(short2, db, false, 1); err == nil {
		t.Fatal("pool acquisition not cancelled")
	}
	held.Close()
	s := testStats(t)
	a := &App{config: c, stats: s, op: func(ctx context.Context, w bool, id int64) error {
		return transaction(ctx, db, w, id)
	}}
	for range 3 {
		a.start()
		time.Sleep(30 * time.Millisecond)
		a.stop()
	}
	a.close()
	// database/sql may finish cancellation-triggered rollback asynchronously.
	waitFor(t, func() bool { return pool.Stats().InUse == 0 })
	if err = db.Exec("DROP TABLE mini_pgbench_accounts").Error; err != nil {
		t.Fatal(err)
	}
	if err = transaction(ctx, db, false, 1); err == nil {
		t.Fatal("missing table error swallowed")
	}
	for _, mode := range []string{"allow", "prefer", "require", "verify-ca", "verify-full"} {
		t.Run(mode, func(t *testing.T) {
			mc := c
			mc.Postgres.SSLMode = mode
			_, p, e := openPostgres(ctx, mc)
			if e != nil {
				t.Fatal(e)
			}
			p.Close()
		})
	}
	c.Postgres.SSLCert = ""
	c.Postgres.SSLKey = ""
	if _, p, e := openPostgres(ctx, c); e == nil {
		p.Close()
		t.Fatal("mTLS accepted without client certificate")
	}
	c.Postgres.SSLMode = "disable"
	if _, p, e := openPostgres(ctx, c); e == nil {
		p.Close()
		t.Fatal("fixture unexpectedly accepts plaintext")
	}
}
