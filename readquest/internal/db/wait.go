// Package db holds helpers shared by the Postgres and ClickHouse clients.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// WaitReady pings a database until it answers or the budget expires.
//
// This exists because ClickHouse Cloud suspends idle services: the first
// connection after a quiet period triggers a wake-up that routinely outlasts
// any timeout you would pick for a healthy service. A single long timeout
// would cover it but gives no feedback for up to a minute; retrying logs
// progress, so a slow start is visibly a wake-up rather than a hang.
//
// The failure mode this guards against is specifically a demo: the service
// idles during the hours before judging, and the first request of the day is
// the one that matters most.
func WaitReady(ctx context.Context, name string, budget, perAttempt time.Duration, ping func(context.Context) error) error {
	deadline := time.Now().Add(budget)

	var lastErr error
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, perAttempt)
		err := ping(attemptCtx)
		cancel()

		if err == nil {
			if attempt > 1 {
				slog.Info("database ready after wake-up", "db", name, "attempts", attempt)
			}
			return nil
		}
		lastErr = err

		// The parent context being done means shutdown, not a slow service.
		if ctx.Err() != nil {
			return fmt.Errorf("waiting for %s: %w", name, ctx.Err())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not ready after %s: %w", name, budget, lastErr)
		}

		slog.Warn("database not ready, retrying (service may be waking from idle)",
			"db", name, "attempt", attempt, "error", err)

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", name, ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
}
