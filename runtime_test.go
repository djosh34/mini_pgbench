package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
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
		t.Skip("started by TestPostgresRuntime")
	}
	os.Args = []string{os.Args[0], "-config", path}
	main()
}

func TestPostgresRuntime(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		t.Run(strconv.FormatBool(fatal), func(t *testing.T) {
			c := pgTestConfig(t)
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
			resp, err := client.Get(url + "/status")
			if err != nil {
				t.Fatal(err)
			}
			var status struct{ Running bool }
			err = json.NewDecoder(resp.Body).Decode(&status)
			resp.Body.Close()
			if err != nil || !status.Running {
				t.Fatalf("not running without POST /start: %+v, %v", status, err)
			}
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
			} else if err = cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
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
