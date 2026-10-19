package main

import (
	"context"
	"database/sql"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openPostgres(ctx context.Context, c Config) (*gorm.DB, *sql.DB, error) {
	db, err := gorm.Open(postgres.Open(c.dsn()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableAutomaticPing: true})
	if err != nil {
		return nil, nil, err
	}
	pool, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	pool.SetMaxOpenConns(c.Pool.MaxOpen)
	pool.SetMaxIdleConns(c.Pool.MaxIdle)
	pool.SetConnMaxLifetime(c.Pool.MaxLifetime)
	pool.SetConnMaxIdleTime(c.Pool.MaxIdleTime)
	if err = pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, nil, err
	}
	return db, pool, nil
}

func initialize(ctx context.Context, db *gorm.DB, rows int64) error {
	if err := db.WithContext(ctx).Exec(`CREATE TABLE IF NOT EXISTS mini_pgbench_accounts (id bigint PRIMARY KEY, balance bigint NOT NULL DEFAULT 0)`).Error; err != nil {
		return err
	}
	return db.WithContext(ctx).Exec(`INSERT INTO mini_pgbench_accounts (id, balance) SELECT generate_series(1, ?::bigint), 0 ON CONFLICT (id) DO NOTHING`, rows).Error
}

func transaction(ctx context.Context, db *gorm.DB, write bool, id int64) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if write {
			return tx.Exec(`UPDATE mini_pgbench_accounts SET balance = balance + 1 WHERE id = ?`, id).Error
		}
		var balance int64
		return tx.Raw(`SELECT balance FROM mini_pgbench_accounts WHERE id = ?`, id).Row().Scan(&balance)
	})
}
