// Command readquest is the ReadQuest MCP server — a gamified reading
// challenge for child literacy, driven entirely through chat.
//
// Topology: this process runs locally and is exposed over HTTPS via ngrok so
// that ClickHouse Cloud's hosted LibreChat ("ClickHouse Agent") can reach it
// as a custom MCP server. Both databases live in ClickHouse Cloud.
//
// Build phases (see PLAN.md):
//
//	Phase 0  configuration + connectivity   <- implemented
//	Phase 1  postgres schema + seed
//	Phase 2  clickhouse schema
//	Phase 3  reading domain (log session, resolvers, XP)
//	Phase 4  gamification (badges, streak)
//	Phase 5  at-risk dashboard (cross-DB merge)
//	Phase 6  MCP server over Streamable HTTP
//	Phase 7  Claude-backed book recommendations
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"readquest/internal/app"
	"readquest/internal/mcpserver"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	checkOnly := flag.Bool("check", false, "verify configuration and database connectivity, then exit")
	migrateOnly := flag.Bool("migrate", false, "apply schema migrations to both databases, then exit")
	flag.Parse()

	if err := run(*checkOnly, *migrateOnly); err != nil {
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

// run wires the application together. It is separate from main so that every
// failure path returns an error to one place rather than calling os.Exit from
// deep in the call stack, leaving deferred cleanup unrun.
func run(checkOnly, migrateOnly bool) error {
	// Cancel on SIGINT/SIGTERM so an interrupted run still closes both pools.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// -check stops at connectivity; every other mode applies migrations, which
	// are idempotent, so the schema can never drift behind the binary.
	a, err := app.Open(ctx, !checkOnly)
	if err != nil {
		return err
	}
	defer a.Close()

	if checkOnly {
		slog.Info("connectivity check passed — both databases reachable")
		return nil
	}

	if migrateOnly {
		slog.Info("migrations complete — both schemas ready")
		return nil
	}

	return mcpserver.New(a).ListenAndServe(ctx, a.Config.ListenAddr, a.Config.APIKey)
}
