package cache

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/RTCMon/rtcmon/internal/config"
)

// ErrMissingURL is returned by NewClient when REDIS_URL is empty.
var ErrMissingURL = errors.New("cache: REDIS_URL is required but was not set")

// NewClient creates a *redis.Client from cfg, verifies connectivity via PING,
// and returns it ready to use. Returns an error (never panics) when:
//   - cfg.URL is empty
//   - the URL cannot be parsed
//   - Redis is unreachable within ctx's deadline
//
// Callers should close the client with client.Close() when done.
func NewClient(ctx context.Context, cfg config.RedisConfig) (*redis.Client, error) {
	if cfg.URL == "" {
		return nil, ErrMissingURL
	}

	opts, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("cache: parse URL: %w", err)
	}

	if cfg.PoolSize > 0 {
		opts.PoolSize = cfg.PoolSize
	}

	client := redis.NewClient(opts)

	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("cache: ping: %w", err)
	}

	return client, nil
}

// Ping sends a lightweight PING command and returns nil when Redis is reachable.
// It is intended for use in health-check handlers.
func Ping(ctx context.Context, client *redis.Client) error {
	return client.Ping(ctx).Err()
}
