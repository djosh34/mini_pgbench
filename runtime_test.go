package main

import (
	"bytes"
	"database/sql"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestRuntimeHelper(t *testing.T) {
	path := os.Getenv("MINI_PGBENCH_TEST_CONFIG")
	if path == "" {
		return
	}
	os.Args = []string{os.Args[0], "-config", path}
	main()
	os.Exit(0)
}

func TestPostgresRuntime(t *testing.T) {
	tlsDir := os.Getenv("TEST_PG_TLS")
	if tlsDir == "" {
		t.Skip("run tests/integration.sh")
	}
	for _, fatal := range []bool{false, true} {
		t.Run(strconv.FormatBool(fatal), func(t *testing.T) {
			c := testConfig()
			c.Postgres.Hosts = []string{"localhost"}
			c.Postgres.Port, _ = strconv.Atoi(os.Getenv("TEST_PG_PORT"))
			c.Postgres.Database = "benchmark"
			c.Postgres.User = "benchmark"
			c.Postgres.SSLMode = "verify-full"
			c.Postgres.SSLRootCert = filepath.Join(tlsDir, "ca.crt")
			c.Postgres.SSLCert = filepath.Join(tlsDir, "client.crt")
			c.Postgres.SSLKey = filepath.Join(tlsDir, "client.key")
			c.Pool.MaxOpen = 2
			c.Pool.MaxIdle = 1
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			c.Listen = listener.Addr().String()
			listener.Close()
			dir := t.TempDir()
			c.SQLitePath = filepath.Join(dir, "results.db")
			data, err := yaml.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config.yaml")
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeHelper$")
			cmd.Env = append(os.Environ(), "MINI_PGBENCH_TEST_CONFIG="+path)
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			client := &http.Client{Timeout: time.Second}
			url := "http://" + c.Listen
			waitFor(t, func() bool {
				resp, e := client.Get(url + "/status")
				if e != nil {
					return false
				}
				resp.Body.Close()
				return resp.StatusCode == 200
			})
			resp, err := client.Post(url+"/start", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if fatal {
				db, e := sql.Open("sqlite", c.SQLitePath)
				if e != nil {
					t.Fatal(e)
				}
				_, e = db.Exec("DROP TABLE summaries")
				db.Close()
				if e != nil {
					t.Fatal(e)
				}
			} else {
				if err = cmd.Process.Signal(os.Interrupt); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err = <-done:
			case <-time.After(8 * time.Second):
				t.Fatal("runtime did not cancel and exit")
			}
			if fatal {
				if err == nil || !strings.Contains(output.String(), "write SQLite summary") {
					t.Fatalf("expected fatal storage failure: %v %s", err, output.String())
				}
			} else if err != nil {
				t.Fatalf("graceful shutdown: %v %s", err, output.String())
			}
		})
	}
}
