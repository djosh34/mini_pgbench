package main

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type App struct {
	mu     sync.Mutex
	config Config
	stats  *Stats
	op     func(ctx context.Context, write bool, id int64) error
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
	for range a.config.Workers {
		a.wg.Go(func() { a.worker(ctx) })
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

func (a *App) stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopLocked()
}

func (a *App) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	a.stopLocked()
}

func (a *App) running() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cancel != nil
}

func chooseWrite(read, write int) bool {
	return rand.IntN(read+write) >= read
}

func (a *App) worker(ctx context.Context) {
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
		// A transaction cut short by a stop is neither a success nor an error.
		if err == nil || ctx.Err() == nil {
			a.stats.record(elapsed, err != nil)
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// resultWriter records whether any part of the response has been sent.
type resultWriter struct {
	http.ResponseWriter
	started bool
}

func (w *resultWriter) Write(p []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(p)
}

func (a *App) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /start", func(w http.ResponseWriter, r *http.Request) {
		if !a.start() {
			jsonResponse(w, http.StatusServiceUnavailable, map[string]string{"error": "shutting down"})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]bool{"running": true})
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		a.stop()
		jsonResponse(w, http.StatusOK, map[string]bool{"running": false})
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, map[string]any{"running": a.running(), "workers": a.config.Workers})
	})
	mux.HandleFunc("GET /results", a.results)
	mux.Handle("GET /metrics", promhttp.HandlerFor(a.stats.registry, promhttp.HandlerOpts{}))
	return mux
}

func (a *App) results(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if q := r.URL.Query(); q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "limit must be a positive integer"})
			return
		}
		limit = n
	}
	w.Header().Set("Content-Type", "application/json")
	stream := &resultWriter{ResponseWriter: w}
	if err := a.stats.results(r.Context(), stream, limit); err != nil {
		if !stream.started {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot read results"})
			return
		}
		// Part of the array is already sent, so a JSON error cannot replace it. Abort the response.
		panic(http.ErrAbortHandler)
	}
}
