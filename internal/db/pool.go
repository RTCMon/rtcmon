// Package db provides a managed PostgreSQL connection pool backed by pgxpool.
// The pool is configured via [config.DBConfig] which maps to the following
// environment variables:
//
//	DB_URL            – required connection string (postgres://…)
//	DB_MAX_CONNS      – maximum pool connections (default 20)
//	DB_MIN_CONNS      – minimum warm connections (default 5)
//	DB_CONN_LIFETIME  – max connection age, e.g. "30m" (default 30m)
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RTCMon/rtcmon/internal/config"
)

// ErrMissingURL is returned by NewPool when DB_URL is empty.
var ErrMissingURL = errors.New("db: DB_URL is required but was not set")

// NewPool creates and verifies a pgxpool.Pool using the supplied DBConfig.
//
// The function returns an error (never panics) when:
//   - cfg.URL is empty
//   - the DSN is malformed
//   - Postgres is unreachable within ctx's deadline
//
// Callers should close the pool with pool.Close() when done.
func NewPool(ctx context.Context, cfg config.DBConfig) (*pgxpool.Pool, error) {
	if cfg.URL == "" {
		return nil, ErrMissingURL
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("db: parse config: %w", err)
	}

	// Apply max/min connection counts.
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = int32(cfg.MaxConns) //nolint:gosec // value is always small
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = int32(cfg.MinConns) //nolint:gosec // value is always small
	}

	// Parse lifetime string (e.g. "30m"). Fall back silently to the pgx
	// default when the value is empty or zero.
	if cfg.ConnLifetime != "" {
		lifetime, err := time.ParseDuration(cfg.ConnLifetime)
		if err != nil {
			return nil, fmt.Errorf("db: parse conn_lifetime %q: %w", cfg.ConnLifetime, err)
		}
		if lifetime > 0 {
			poolCfg.MaxConnLifetime = lifetime
			// Use the same value for idle lifetime so long-idle connections
			// are also replaced promptly.
			poolCfg.MaxConnIdleTime = lifetime
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}

	// Eagerly verify connectivity so callers know the pool is usable.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}

	return pool, nil
}

// Ping sends a lightweight ping to Postgres and returns nil when the database
// is reachable. It is intended for use in health-check handlers.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	return pool.Ping(ctx)
}
