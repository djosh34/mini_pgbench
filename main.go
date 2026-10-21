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
	path := flag.String("config", "config.yaml", "path to the YAML config file")
	flag.Parse()
	c, err := loadConfig(*path)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := openPostgres(ctx, c)
	if err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}
	defer pool.Close()
	if err = initialize(ctx, pool, c.Rows); err != nil {
		return fmt.Errorf("initialize benchmark table: %w", err)
	}
	// The history starts at process start, so the seconds spent connecting show up as idle rows.
	stats, err := newStats(c.SQLitePath, started, pool)
	if err != nil {
		return fmt.Errorf("initialize SQLite history: %w", err)
	}
	defer stats.close()
	app := &App{
		config: c,
		stats:  stats,
		op: func(ctx context.Context, write bool, id int64) error {
			return transaction(ctx, pool, write, id)
		},
	}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	app.start()
	server := &http.Server{Handler: app.handler(), ReadHeaderTimeout: 5 * time.Second}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	collectCtx, cancelCollect := context.WithCancel(context.Background())
	defer cancelCollect()
	collectDone := make(chan error, 1)
	go func() {
		collectDone <- stats.collect(collectCtx)
		close(collectDone)
	}()
	log.Printf("listening on %s; workload running", listener.Addr())
	select {
	case <-ctx.Done():
	case err = <-collectDone:
	case err = <-serverDone:
	}
	app.close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
		_ = server.Close()
	}
	cancelCollect()
	// If the select above already took the collector's result, this receives nil from the closed channel.
	if collectErr := <-collectDone; collectErr != nil {
		err = collectErr
	}
	return err
}

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
