// Package app wires configuration, both database handles, and the domain
// stores into one value.
//
// It exists so the MCP server and the CLI open the application identically —
// the CLI is not a toy harness against a parallel setup, it drives exactly
// what the server drives. That is what makes it a usable fallback if the
// hosted chat UI is unreachable during a demo.
package app

import (
	"context"

	"readquest/internal/ai"
	"readquest/internal/config"
	"readquest/internal/db/clickhouse"
	"readquest/internal/db/postgres"
	"readquest/internal/domain/dashboard"
	"readquest/internal/domain/reading"
	"readquest/migrations"
)

type App struct {
	Config *config.Config
	PG     *postgres.DB
	CH     *clickhouse.DB

	Reading   *reading.Store
	Dashboard *dashboard.Store

	// Recommender is nil when ANTHROPIC_API_KEY is unset. Every other feature
	// works without it, so a missing key degrades one tool rather than
	// blocking startup.
	Recommender *ai.Recommender
}

// Open loads configuration, connects to both databases, and optionally applies
// migrations.
//
// Migrations are idempotent, so running them on every server boot keeps the
// schema from drifting behind the binary. The CLI passes false: it is often
// used to inspect state, and silently reseeding during an inspection would be
// its own kind of confusing.
func Open(ctx context.Context, migrate bool) (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	pg, err := postgres.Connect(ctx, cfg.PostgresDSN)
	if err != nil {
		return nil, err
	}

	ch, err := clickhouse.Connect(ctx, cfg.ClickHouseDSN)
	if err != nil {
		pg.Close()
		return nil, err
	}

	if migrate {
		if err := migrations.RunPostgres(ctx, pg.Pool); err != nil {
			pg.Close()
			_ = ch.Close()
			return nil, err
		}
		if err := migrations.RunClickHouse(ctx, ch.Conn); err != nil {
			pg.Close()
			_ = ch.Close()
			return nil, err
		}
	}

	return &App{
		Config:    cfg,
		PG:        pg,
		CH:        ch,
		Reading:     reading.NewStore(pg, ch),
		Dashboard:   dashboard.NewStore(pg, ch),
		Recommender: ai.NewRecommender(cfg.AnthropicKey),
	}, nil
}

func (a *App) Close() {
	a.PG.Close()
	_ = a.CH.Close()
}
