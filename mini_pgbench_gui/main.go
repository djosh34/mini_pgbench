// mini_pgbench_gui serves the mini_pgbench web GUI and forwards /api to the benchmark API.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"
)

//go:embed dist
var dist embed.FS

func main() {
	api := flag.String("api", "http://localhost:8080", "benchmark API URL, usually the local end of kubectl port-forward")
	listen := flag.String("listen", "127.0.0.1:5173", "address for the GUI")
	flag.Parse()
	if err := run(*api, *listen); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(api, listen string) error {
	h, err := handler(api)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	fmt.Printf("mini_pgbench GUI for %s: open http://%s\n", api, listener.Addr())
	server := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	return server.Serve(listener)
}

func handler(api string) (http.Handler, error) {
	target, err := url.Parse(api)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return nil, fmt.Errorf("-api must be an http or https URL such as http://localhost:8080, got %q", api)
	}
	files, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) },
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("Cannot reach the benchmark API at %s: %v", api, err)})
		},
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", proxy))
	mux.Handle("/", http.FileServerFS(files))
	return mux, nil
}
