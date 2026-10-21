package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.yaml.in/yaml/v3"
)

type Config struct {
	Postgres struct {
		Hosts       []string `yaml:"hosts"`
		Port        int      `yaml:"port"`
		Database    string   `yaml:"database"`
		User        string   `yaml:"user"`
		Password    string   `yaml:"password"`
		SSLMode     string   `yaml:"sslmode"`
		SSLRootCert string   `yaml:"sslrootcert"`
		SSLCert     string   `yaml:"sslcert"`
		SSLKey      string   `yaml:"sslkey"`
	} `yaml:"postgres"`
	Pool struct {
		MaxOpen     int           `yaml:"max_open"`
		MaxIdle     int           `yaml:"max_idle"`
		MaxLifetime time.Duration `yaml:"max_lifetime"`
		MaxIdleTime time.Duration `yaml:"max_idle_time"`
	} `yaml:"pool"`
	Workers            int           `yaml:"workers"`
	Rows               int64         `yaml:"rows"`
	ReadWeight         int           `yaml:"read_weight"`
	WriteWeight        int           `yaml:"write_weight"`
	TransactionTimeout time.Duration `yaml:"transaction_timeout"`
	Listen             string        `yaml:"listen"`
	SQLitePath         string        `yaml:"sqlite_path"`
}

// clearPostgresEnv unsets the PG* environment variables that pgx reads, so a
// shell with PGSERVICE or PGOPTIONS set cannot change the connection.
func clearPostgresEnv() {
	for _, key := range []string{
		"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGPASSFILE",
		"PGAPPNAME", "PGCONNECT_TIMEOUT", "PGSSLMODE", "PGSSLKEY", "PGSSLCERT",
		"PGSSLSNI", "PGSSLROOTCERT", "PGSSLPASSWORD", "PGSSLNEGOTIATION",
		"PGTARGETSESSIONATTRS", "PGSERVICE", "PGSERVICEFILE", "PGTZ", "PGOPTIONS",
		"PGMINPROTOCOLVERSION", "PGMAXPROTOCOLVERSION", "PGCHANNELBINDING", "PGREQUIREAUTH",
	} {
		_ = os.Unsetenv(key)
	}
}

func loadConfig(path string) (Config, error) {
	var c Config
	c.Postgres.Port = 5432
	c.Postgres.SSLMode = "verify-full"
	c.Pool.MaxOpen, c.Pool.MaxIdle = 10, 10
	c.Workers, c.Rows, c.ReadWeight, c.WriteWeight = 4, 100000, 80, 20
	c.Listen, c.SQLitePath = ":8080", "results.db"
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err = dec.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("expected one YAML document")
	}
	return c, c.validate()
}

func (c Config) validate() error {
	p := c.Postgres
	if len(p.Hosts) == 0 || p.Database == "" || p.User == "" ||
		p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("postgres hosts, database, user and valid port required")
	}
	for _, h := range p.Hosts {
		if strings.TrimSpace(h) == "" || strings.Contains(h, ",") {
			return fmt.Errorf("each host must be a nonempty single hostname or address")
		}
	}
	if c.Workers < 1 || c.Rows < 1 ||
		c.Pool.MaxOpen < 1 || c.Pool.MaxIdle < 0 || c.Pool.MaxIdle > c.Pool.MaxOpen {
		return fmt.Errorf("invalid worker, row or pool counts")
	}
	if c.ReadWeight < 0 || c.WriteWeight < 0 ||
		c.ReadWeight > math.MaxInt-c.WriteWeight || c.ReadWeight+c.WriteWeight == 0 {
		return fmt.Errorf("weights must be nonnegative with a positive, non-overflowing sum")
	}
	if c.TransactionTimeout < 0 || c.Pool.MaxLifetime < 0 || c.Pool.MaxIdleTime < 0 {
		return fmt.Errorf("durations must be nonnegative")
	}
	if c.Listen == "" || c.SQLitePath == "" ||
		c.SQLitePath == ":memory:" || strings.HasPrefix(c.SQLitePath, "file:") {
		return fmt.Errorf("listen and a filesystem SQLite path required")
	}
	_, err := pgx.ParseConfig(c.dsn())
	return err
}

func (c Config) dsn() string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	quote := func(s string) string { return "'" + r.Replace(s) + "'" }
	p := c.Postgres
	// Empty sslrootcert, sslcert and sslkey stop pgx from falling back to the
	// files in ~/.postgresql.
	return fmt.Sprintf(
		"host=%s port=%d dbname=%s user=%s password=%s "+
			"sslmode=%s sslrootcert=%s sslcert=%s sslkey=%s "+
			"connect_timeout=0 application_name=mini_pgbench",
		quote(strings.Join(p.Hosts, ",")),
		p.Port,
		quote(p.Database),
		quote(p.User),
		quote(p.Password),
		quote(p.SSLMode),
		quote(p.SSLRootCert),
		quote(p.SSLCert),
		quote(p.SSLKey),
	)
}
