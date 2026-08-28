// Package clickhouse owns ReadQuest's analytical event stream.
//
// Every reading session is mirrored here as a reading_events row. ClickHouse
// answers the teacher at-risk dashboard and backs judges' ad-hoc questions via
// ClickHouse Cloud's native MCP. It is deliberately NOT the source of truth:
// Postgres owns state, and a write failure here is logged and swallowed rather
// than surfaced to the user.
package clickhouse

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"readquest/internal/db"
)

// A suspended ClickHouse Cloud service can take the better part of a minute to
// resume. These bound how long startup waits for that.
const (
	readyBudget     = 90 * time.Second
	readyPerAttempt = 20 * time.Second
)

type DB struct {
	Conn driver.Conn
}

// Connect opens a connection and verifies it with a ping.
//
// The protocol comes from the DSN scheme — https:// selects HTTP on 8443,
// clickhouse:// selects native on 9440 — and either works. .env.example
// defaults to HTTPS because 9440 is a non-standard port that restricted
// networks tend to block, and ReadQuest's write volume is far too low for
// native's efficiency advantage to register.
//
// secure=true is required with both schemes and is never inferred from the
// scheme, so a DSN missing it fails at parse rather than at dial.
func Connect(ctx context.Context, dsn string) (*DB, error) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing clickhouse DSN: %w", err)
	}

	opts.DialTimeout = readyPerAttempt
	opts.MaxOpenConns = 8

	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("opening clickhouse connection: %w", err)
	}

	// clickhouse.Open is lazy — it does not talk to the server. The wait below
	// is what actually establishes (and, if suspended, wakes) the service.
	chdb := &DB{Conn: conn}
	if err := db.WaitReady(ctx, "clickhouse", readyBudget, readyPerAttempt, chdb.Ping); err != nil {
		conn.Close()
		return nil, err
	}

	var host string
	if len(opts.Addr) > 0 {
		host = opts.Addr[0]
	}
	slog.Info("clickhouse connected", "host", host, "database", opts.Auth.Database)
	return chdb, nil
}

// Ping honours the caller's deadline rather than imposing its own, so that
// WaitReady controls the per-attempt budget in one place.
func (db *DB) Ping(ctx context.Context) error {
	if err := db.Conn.Ping(ctx); err != nil {
		return fmt.Errorf("pinging clickhouse: %w", err)
	}
	return nil
}

func (db *DB) Close() error {
	err := db.Conn.Close()
	slog.Info("clickhouse connection closed")
	return err
}
