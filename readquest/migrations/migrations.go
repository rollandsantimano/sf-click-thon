// Package migrations applies ReadQuest's schema to both databases.
//
// The runner lives beside the .sql files because go:embed cannot reach into a
// parent directory — an embed in internal/db could not see migrations/.
//
// There is no schema_migrations tracking table. Every statement is written to
// be idempotent (CREATE ... IF NOT EXISTS, ON CONFLICT DO NOTHING) and the
// full set re-runs on each startup. For a one-day build that trades a little
// startup time for the ability to restart the process freely mid-demo.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed postgres/*.sql
var postgresFS embed.FS

//go:embed clickhouse/*.sql
var clickhouseFS embed.FS

// RunPostgres applies every postgres/*.sql file in filename order.
func RunPostgres(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := sqlFiles(postgresFS, "postgres")
	if err != nil {
		return err
	}

	for _, f := range files {
		body, err := postgresFS.ReadFile(f)
		if err != nil {
			return fmt.Errorf("reading %s: %w", f, err)
		}

		// QueryExecModeSimpleProtocol routes through pgConn.Exec, which reads
		// multiple results and so accepts a multi-statement script. The
		// default extended protocol rejects anything past the first statement.
		if _, err := pool.Exec(ctx, string(body), pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("applying %s: %w", f, err)
		}
		slog.Info("postgres migration applied", "file", f)
	}

	slog.Info("postgres schema ready", "files", len(files))
	logSeedSummary(ctx, pool)
	return nil
}

// logSeedSummary reports what the seed actually produced.
//
// Idempotent seeds fail quietly: an ON CONFLICT DO NOTHING that skips every
// row looks identical to one that inserted them. Printing the counts turns
// "the migration ran" into "the data is there", which is the claim that
// matters before building on top of it.
func logSeedSummary(ctx context.Context, pool *pgxpool.Pool) {
	var classes, teachers, students, books, badges int
	err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM classes),
		       (SELECT count(*) FROM teachers),
		       (SELECT count(*) FROM students),
		       (SELECT count(*) FROM books),
		       (SELECT count(*) FROM badges)`,
	).Scan(&classes, &teachers, &students, &books, &badges)
	if err != nil {
		// Not fatal: the schema is applied either way, and a failed count
		// should not stop the server from starting.
		slog.Warn("could not read seed summary", "error", err)
		return
	}

	slog.Info("seed data present",
		"classes", classes, "teachers", teachers,
		"students", students, "books", books, "badges", badges)
}

// RunClickHouse applies every clickhouse/*.sql file in filename order.
//
// Each file must contain exactly ONE statement: the ClickHouse driver's Exec
// does not accept a multi-statement script, and splitting on ";" would break
// on any semicolon inside a string literal.
func RunClickHouse(ctx context.Context, conn driver.Conn) error {
	files, err := sqlFiles(clickhouseFS, "clickhouse")
	if err != nil {
		return err
	}

	for _, f := range files {
		body, err := clickhouseFS.ReadFile(f)
		if err != nil {
			return fmt.Errorf("reading %s: %w", f, err)
		}

		stmt := strings.TrimRight(strings.TrimSpace(string(body)), ";")
		if stmt == "" {
			continue
		}
		if err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("applying %s: %w", f, err)
		}
		slog.Info("clickhouse migration applied", "file", f)
	}

	slog.Info("clickhouse schema ready", "files", len(files))
	return nil
}

// sqlFiles lists dir/*.sql. fs.ReadDir sorts by filename, which is what gives
// the 001_, 002_ prefixes their meaning.
func sqlFiles(fsys embed.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("listing %s migrations: %w", dir, err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, dir+"/"+e.Name())
		}
	}
	return files, nil
}
