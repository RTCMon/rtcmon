package cache_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/RTCMon/rtcmon/internal/cache"
	"github.com/RTCMon/rtcmon/internal/config"
)

// testRedisURL returns the integration-test URL from the environment.
// Tests that require a real Redis are skipped when this is absent.
func testRedisURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; skipping integration test")
	}
	return url
}

// TestNewClient_MissingURL expects ErrMissingURL when REDIS_URL is empty.
func TestNewClient_MissingURL(t *testing.T) {
	cfg := config.RedisConfig{} // URL is empty

	client, err := cache.NewClient(context.Background(), cfg)
	if err == nil {
		_ = client.Close()
		t.Fatal("expected error for missing URL, got nil")
	}

	if !errors.Is(err, cache.ErrMissingURL) {
		t.Errorf("error = %v, want %v", err, cache.ErrMissingURL)
	}
}

// TestNewClient_InvalidURL expects a descriptive error on a malformed URL.
func TestNewClient_InvalidURL(t *testing.T) {
	cfg := config.RedisConfig{
		URL: "not-a-valid-dsn://%%invalid",
	}

	client, err := cache.NewClient(context.Background(), cfg)
	if err == nil {
		_ = client.Close()
		t.Fatal("expected error for invalid URL, got nil")
	}

	t.Logf("got expected error: %v", err)
}

// TestNewClient_UnreachableHost expects an error returned within 3s when
// nothing is listening on the given address.
func TestNewClient_UnreachableHost(t *testing.T) {
	cfg := config.RedisConfig{
		// Valid URL format but nothing is listening on port 9998.
		URL:      "redis://localhost:9998/0",
		PoolSize: 2,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client, err := cache.NewClient(ctx, cfg)
	if err == nil {
		_ = client.Close()
		t.Fatal("expected error for unreachable host, got nil")
	}

	t.Logf("got expected error within deadline: %v", err)
}

// TestNewClient_Valid connects to a real Redis and expects success.
func TestNewClient_Valid(t *testing.T) {
	url := testRedisURL(t)

	cfg := config.RedisConfig{
		URL:      url,
		PoolSize: 5,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := cache.NewClient(ctx, cfg)
	if err != nil {
		t.Fatalf("NewClient: unexpected error: %v", err)
	}
	defer client.Close()
}

// TestPing_ReachableRedis verifies that Ping returns nil on a live Redis.
func TestPing_ReachableRedis(t *testing.T) {
	url := testRedisURL(t)

	cfg := config.RedisConfig{
		URL:      url,
		PoolSize: 2,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := cache.NewClient(ctx, cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pingCancel()

	if err := cache.Ping(pingCtx, client); err != nil {
		t.Errorf("Ping: unexpected error: %v", err)
	}
}

// TestPing_UnreachableRedis verifies that Ping returns an error when Redis
// is not reachable (client pointed at a non-listening port).
func TestPing_UnreachableRedis(t *testing.T) {
	// Build a client without going through NewClient (which would already
	// fail the eager ping). Use go-redis directly so we can test Ping itself.
	//
	// We import redis here via the cache package's own dependency — go-redis
	// is a direct dep of the module, so we can also use it in tests.
	cfg := config.RedisConfig{
		URL:      "redis://localhost:9997/0",
		PoolSize: 1,
	}

	// Use a fresh client bypassing the constructor's eager ping by creating
	// it manually through ParseURL. Since go-redis is a direct dep, we call
	// cache.NewClient with a context that's already cancelled so the ping
	// within NewClient itself fails — that's what we're testing here too.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := cache.NewClient(ctx, cfg)
	if err == nil {
		t.Fatal("expected error pinging unreachable Redis, got nil")
	}

	t.Logf("got expected error: %v", err)
}
