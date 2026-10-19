package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func run() error {
	started := time.Now()
	clearPostgresEnv()
	path := flag.String("config", "config.yaml", "static YAML configuration path")
	flag.Parse()
	c, err := loadConfig(*path)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, pool, err := openPostgres(ctx, c)
	if err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}
	defer pool.Close()
	if err = initialize(ctx, db, c.Rows); err != nil {
		return fmt.Errorf("initialize benchmark table: %w", err)
	}
	stats, err := newStats(c.SQLitePath, pool)
	if err != nil {
		return fmt.Errorf("initialize SQLite history: %w", err)
	}
	defer stats.history.Close()
	stats.epoch = started // Initialization seconds are recorded as idle, never benchmark samples.
	app := &App{config: c, stats: stats, op: func(ctx context.Context, write bool, id int64) error { return transaction(ctx, db, write, id) }}
	defer app.close()
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: app.handler(), ReadHeaderTimeout: 5 * time.Second}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	collectCtx, cancelCollect := context.WithCancel(context.Background())
	defer cancelCollect()
	collectDone := make(chan error, 1)
	go func() { collectDone <- stats.collect(collectCtx) }()
	log.Printf("listening on %s; workload paused", listener.Addr())
	collectorExited := false
	select {
	case <-ctx.Done():
	case err = <-collectDone:
		collectorExited = true
	case err = <-serverDone:
		if err == http.ErrServerClosed {
			err = nil
		}
	}
	app.close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
		_ = server.Close()
	}
	cancelCollect()
	if !collectorExited {
		if collectErr := <-collectDone; collectErr != nil {
			err = collectErr
		}
	}
	return err
}
func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
