package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestPostgres(t *testing.T) {
	dir := os.Getenv("TEST_PG_TLS")
	if dir == "" {
		t.Skip("run tests/integration.sh for disposable mTLS PostgreSQL")
	}
	c := testConfig()
	c.Postgres.Hosts = []string{"localhost", "127.0.0.1"}
	c.Postgres.Port, _ = strconv.Atoi(os.Getenv("TEST_PG_PORT"))
	c.Postgres.Database = "benchmark"
	c.Postgres.User = "benchmark"
	c.Postgres.SSLMode = "verify-full"
	c.Postgres.SSLRootCert = filepath.Join(dir, "ca.crt")
	c.Postgres.SSLCert = filepath.Join(dir, "client.crt")
	c.Postgres.SSLKey = filepath.Join(dir, "client.key")
	c.Pool.MaxOpen = 2
	c.Pool.MaxIdle = 1
	c.Pool.MaxLifetime = time.Minute
	c.Pool.MaxIdleTime = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	db.Raw("SELECT balance FROM mini_pgbench_accounts WHERE id=1").Scan(&balance)
	if balance != 1 {
		t.Fatal("initialization reset data")
	}
	var count int64
	db.Raw("SELECT count(*) FROM mini_pgbench_accounts").Scan(&count)
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
	a := &App{config: c, stats: s, op: func(ctx context.Context, w bool, id int64) error { return transaction(ctx, db, w, id) }}
	for i := 0; i < 3; i++ {
		a.start()
		time.Sleep(30 * time.Millisecond)
		a.stop()
	}
	a.close()
	if pool.Stats().InUse != 0 {
		t.Fatal("workers left connections in use")
	}
	if err = db.Exec("DROP TABLE mini_pgbench_accounts").Error; err != nil {
		t.Fatal(err)
	}
	if err = transaction(ctx, db, false, 1); err == nil {
		t.Fatal("missing table error swallowed")
	}
	for _, mode := range []string{"allow", "prefer", "require", "verify-ca", "verify-full"} {
		t.Run(mode, func(t *testing.T) {
			copy := c
			copy.Postgres.SSLMode = mode
			_, p, e := openPostgres(ctx, copy)
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
