package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	body, err := io.ReadAll(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	return w.Code, string(body)
}

func TestHandler(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.URL.RequestURI())
	}))
	defer api.Close()
	h, err := handler(api.URL)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := get(t, h, "/"); code != 200 || !strings.Contains(body, `<div id="app">`) {
		t.Fatalf("index: %d %s", code, body)
	}
	if code, body := get(t, h, "/api/results?limit=5"); code != 200 || body != "/results?limit=5" {
		t.Fatalf("proxy: %d %s", code, body)
	}
	api.Close()
	if code, body := get(t, h, "/api/status"); code != 502 || !strings.Contains(body, "Cannot reach the benchmark API") {
		t.Fatalf("API down: %d %s", code, body)
	}
}

func TestBadAPIFlag(t *testing.T) {
	for _, api := range []string{"localhost:8080", "ftp://x", "http://"} {
		if _, err := handler(api); err == nil {
			t.Fatalf("%q accepted", api)
		}
	}
}
