// Package config loads ReadQuest's runtime configuration from the environment.
//
// Credentials never live in the repository: .env is gitignored and
// .env.example documents the shape callers must fill in.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	PostgresDSN   string
	ClickHouseDSN string

	// AnthropicKey powers recommend_book (Phase 7). Not required to boot, so
	// that Phases 0-6 can be built and demoed without a Claude API key.
	AnthropicKey string

	// APIKey is the shared secret LibreChat presents as X-API-Key (Phase 6).
	// Same reasoning as AnthropicKey: not required until the server exists.
	APIKey string

	ListenAddr string
}

// Load reads .env if present, then the process environment.
//
// Values already exported in the shell win over .env, so a one-off override
// (e.g. pointing at a scratch database) needs no file edit.
func Load() (*Config, error) {
	// Absent .env is not an error: in a deployed context the values arrive
	// through the environment and no file exists.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		// A malformed .env is worth reporting — silently ignoring it would
		// surface later as a confusing "missing credential" error.
		if !strings.Contains(err.Error(), "no such file") {
			return nil, fmt.Errorf("parsing .env: %w", err)
		}
	}

	cfg := &Config{
		PostgresDSN:   os.Getenv("POSTGRES_DSN"),
		ClickHouseDSN: os.Getenv("CLICKHOUSE_DSN"),
		AnthropicKey:  os.Getenv("ANTHROPIC_API_KEY"),
		APIKey:        os.Getenv("READQUEST_API_KEY"),
		ListenAddr:    envOr("LISTEN_ADDR", ":8080"),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate checks only what is needed to boot. Phase-gated credentials are
// checked at their point of use so an incomplete .env does not block earlier
// phases.
func (c *Config) validate() error {
	var missing []string
	if c.PostgresDSN == "" {
		missing = append(missing, "POSTGRES_DSN")
	}
	if c.ClickHouseDSN == "" {
		missing = append(missing, "CLICKHOUSE_DSN")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required environment variables: %s (see .env.example)",
			strings.Join(missing, ", "))
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
