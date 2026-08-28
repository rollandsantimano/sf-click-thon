// Package postgres owns ReadQuest's relational state: students, books,
// sessions, badges, and the authoritative streak counter.
//
// Postgres is the source of truth. ClickHouse (see internal/db/clickhouse)
// holds a parallel event stream used only for analytics — a failed ClickHouse
// write must never fail a Postgres-backed operation.
package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"readquest/internal/db"
)

// ClickHouse Cloud's Postgres has resumed fast in practice, but it is subject
// to the same idle-suspend behaviour, so startup waits rather than assuming.
const (
	readyBudget     = 60 * time.Second
	readyPerAttempt = 15 * time.Second
)

type DB struct {
	Pool *pgxpool.Pool
}

// Connect opens a pooled connection and verifies it with a ping.
//
// Connecting eagerly (rather than lazily on first query) means a bad DSN
// fails at startup with a clear message instead of surfacing mid-demo.
func Connect(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing postgres DSN: %w", err)
	}

	// Small pool: this is a single-process hackathon server whose concurrency
	// is bounded by chat turns, not by throughput.
	cfg.MaxConns = 8
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}

	pgdb := &DB{Pool: pool}
	if err := db.WaitReady(ctx, "postgres", readyBudget, readyPerAttempt, pgdb.Ping); err != nil {
		pool.Close()
		return nil, err
	}

	slog.Info("postgres connected", "host", cfg.ConnConfig.Host, "database", cfg.ConnConfig.Database)
	return pgdb, nil
}

// Ping honours the caller's deadline rather than imposing its own, so that
// WaitReady controls the per-attempt budget in one place.
func (db *DB) Ping(ctx context.Context) error {
	if err := db.Pool.Ping(ctx); err != nil {
		return fmt.Errorf("pinging postgres: %w", err)
	}
	return nil
}

func (db *DB) Close() {
	db.Pool.Close()
	slog.Info("postgres connection closed")
}
