package db_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/RTCMon/rtcmon/internal/config"
	"github.com/RTCMon/rtcmon/internal/db"
)

// testDBURL returns the integration-test DSN from the environment.
// Tests that require a real Postgres are skipped when this is absent.
func testDBURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("TEST_DB_URL not set; skipping integration test")
	}
	return url
}

// TestNewPool_ValidURL connects to a real Postgres and expects success.
func TestNewPool_ValidURL(t *testing.T) {
	url := testDBURL(t)

	cfg := config.DBConfig{
		URL:          url,
		MaxConns:     5,
		MinConns:     1,
		ConnLifetime: "30m",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPool: unexpected error: %v", err)
	}
	defer pool.Close()

	stat := pool.Stat()
	if stat.TotalConns() < 0 {
		t.Errorf("TotalConns: got %d, want >= 0", stat.TotalConns())
	}
}

// TestNewPool_InvalidURL expects a descriptive error on a malformed DSN.
func TestNewPool_InvalidURL(t *testing.T) {
	cfg := config.DBConfig{
		URL:      "not-a-valid-dsn://%%invalid",
		MaxConns: 5,
		MinConns: 1,
	}

	ctx := context.Background()
	pool, err := db.NewPool(ctx, cfg)
	if err == nil {
		pool.Close()
		t.Fatal("expected error for invalid DSN, got nil")
	}

	t.Logf("got expected error: %v", err)
}

// TestNewPool_UnreachableHost expects an error returned within 5 s when
// Postgres is not listening on the given address.
func TestNewPool_UnreachableHost(t *testing.T) {
	cfg := config.DBConfig{
		// Valid DSN format but nothing is listening on port 9999.
		URL:          "postgres://postgres:pass@localhost:9999/testdb?connect_timeout=3",
		MaxConns:     2,
		MinConns:     1,
		ConnLifetime: "30m",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, cfg)
	if err == nil {
		pool.Close()
		t.Fatal("expected error for unreachable host, got nil")
	}

	t.Logf("got expected error within deadline: %v", err)
}

// TestNewPool_MissingURL expects ErrMissingURL when DB_URL is empty.
func TestNewPool_MissingURL(t *testing.T) {
	cfg := config.DBConfig{} // URL is empty

	pool, err := db.NewPool(context.Background(), cfg)
	if err == nil {
		pool.Close()
		t.Fatal("expected error for missing URL, got nil")
	}

	if !errors.Is(err, db.ErrMissingURL) {
		t.Errorf("error = %v, want %v", err, db.ErrMissingURL)
	}
}

// TestPing_AfterConnect connects to a real Postgres and verifies that Ping
// returns nil immediately after pool creation.
func TestPing_AfterConnect(t *testing.T) {
	url := testDBURL(t)

	cfg := config.DBConfig{
		URL:          url,
		MaxConns:     5,
		MinConns:     1,
		ConnLifetime: "30m",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pingCancel()

	if err := db.Ping(pingCtx, pool); err != nil {
		t.Errorf("Ping: unexpected error: %v", err)
	}
}
