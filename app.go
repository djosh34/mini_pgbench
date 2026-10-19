package main

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type operation func(context.Context, bool, int64) error

type App struct {
	mu     sync.Mutex
	config Config
	stats  *Stats
	op     operation
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed bool
}

func (a *App) start() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return false
	}
	if a.cancel != nil {
		return true
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.stats.running.Set(1)
	for i := 0; i < a.config.Workers; i++ {
		a.wg.Add(1)
		go a.worker(ctx)
	}
	return true
}

func (a *App) stopLocked() {
	if a.cancel != nil {
		a.cancel()
		a.wg.Wait()
		a.cancel = nil
	}
	a.stats.running.Set(0)
}
func (a *App) stop()                   { a.mu.Lock(); defer a.mu.Unlock(); a.stopLocked() }
func (a *App) close()                  { a.mu.Lock(); defer a.mu.Unlock(); a.closed = true; a.stopLocked() }
func (a *App) running() bool           { a.mu.Lock(); defer a.mu.Unlock(); return a.cancel != nil }
func chooseWrite(read, write int) bool { return rand.IntN(read+write) >= read }

func (a *App) worker(ctx context.Context) {
	defer a.wg.Done()
	for ctx.Err() == nil {
		write := chooseWrite(a.config.ReadWeight, a.config.WriteWeight)
		id := rand.Int64N(a.config.Rows) + 1
		txctx := ctx
		cancel := func() {}
		if a.config.TransactionTimeout > 0 {
			txctx, cancel = context.WithTimeout(ctx, a.config.TransactionTimeout)
		}
		start := time.Now()
		err := a.op(txctx, write, id)
		elapsed := time.Since(start)
		cancel()
		if err == nil {
			a.stats.record(elapsed, false)
		} else if ctx.Err() == nil {
			a.stats.record(elapsed, true)
		}
		if err != nil {
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type resultWriter struct {
	http.ResponseWriter
	started bool
}

func (w *resultWriter) Write(p []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(p)
}

func (a *App) handler() http.Handler {
	metrics := promhttp.HandlerFor(a.stats.registry, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := "GET"
		if r.URL.Path == "/start" || r.URL.Path == "/stop" {
			method = "POST"
		}
		switch r.URL.Path {
		case "/start", "/stop", "/status", "/results", "/metrics":
		default:
			jsonResponse(w, 404, map[string]string{"error": "not found"})
			return
		}
		if r.Method != method {
			w.Header().Set("Allow", method)
			jsonResponse(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
		switch r.URL.Path {
		case "/start":
			if !a.start() {
				jsonResponse(w, 503, map[string]string{"error": "shutting down"})
				return
			}
			jsonResponse(w, 200, map[string]bool{"running": true})
		case "/stop":
			a.stop()
			jsonResponse(w, 200, map[string]bool{"running": false})
		case "/status":
			jsonResponse(w, 200, map[string]any{"running": a.running(), "workers": a.config.Workers})
		case "/results":
			w.Header().Set("Content-Type", "application/json")
			stream := &resultWriter{ResponseWriter: w}
			if err := a.stats.results(r.Context(), stream); err != nil {
				if !stream.started {
					jsonResponse(w, 500, map[string]string{"error": "cannot read results"})
					return
				}
				// A partial streamed response cannot be replaced with JSON; abort it.
				panic(http.ErrAbortHandler)
			}
		case "/metrics":
			metrics.ServeHTTP(w, r)
		}
	})
}
