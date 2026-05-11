// Package testutil provides shared helpers for integration tests. It starts
// real Postgres and Redis containers via testcontainers-go, applies the full
// migration schema, and exposes seed helpers for common fixtures.
//
// Functions in this package are only called from test files tagged
// //go:build integration, but the package itself carries no build tag so it
// compiles on every build (keeping the module graph consistent).
package testutil

import (
	"context"
	"fmt"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/RTCMon/rtcmon/internal/migrate"
)

const (
	pgImage    = "postgres:16-alpine"
	redisImage = "redis:7-alpine"
)

// StartPostgresForMain starts a Postgres container suitable for use in
// TestMain (where testing.TB is unavailable). It returns the DSN and a
// teardown function the caller must invoke before os.Exit.
// Migrations are applied before returning.
func StartPostgresForMain(ctx context.Context) (connStr string, teardown func()) {
	ctr, err := tcpostgres.Run(ctx, pgImage,
		tcpostgres.WithDatabase("rtcmon_test"),
		tcpostgres.WithUsername("rtcmon"),
		tcpostgres.WithPassword("rtcmon"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		panic("testutil: start postgres for TestMain: " + err.Error())
	}

	str, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = ctr.Terminate(ctx)
		panic("testutil: postgres connection string: " + err.Error())
	}

	if err := migrate.Up(ctx, str); err != nil {
		_ = ctr.Terminate(ctx)
		panic("testutil: migrate up: " + err.Error())
	}

	teardown = func() { _ = ctr.Terminate(ctx) }
	return str, teardown
}

// StartPostgres starts a Postgres 16 container, runs all migrations, and
// returns a ready *pgxpool.Pool. The container is terminated on t.Cleanup.
func StartPostgres(t testing.TB) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx, pgImage,
		tcpostgres.WithDatabase("rtcmon_test"),
		tcpostgres.WithUsername("rtcmon"),
		tcpostgres.WithPassword("rtcmon"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("testutil: start postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(ctx); err != nil {
			t.Logf("testutil: terminate postgres: %v", err)
		}
	})

	connStr, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("testutil: postgres connection string: %v", err)
	}

	if err := migrate.Up(ctx, connStr); err != nil {
		t.Fatalf("testutil: migrate up: %v", err)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("testutil: pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

// StartRedis starts a Redis 7 container and returns a ready *redis.Client.
// The container is terminated on t.Cleanup.
func StartRedis(t testing.TB) *redis.Client {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcredis.Run(ctx, redisImage)
	if err != nil {
		t.Fatalf("testutil: start redis: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(ctx); err != nil {
			t.Logf("testutil: terminate redis: %v", err)
		}
	})

	addr, err := ctr.Endpoint(ctx, "")
	if err != nil {
		t.Fatalf("testutil: redis endpoint: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { rdb.Close() })

	return rdb
}

// SeedApp inserts a minimal org and app row and returns (orgID, appID).
func SeedApp(t testing.TB, pool *pgxpool.Pool) (orgID, appID int64) {
	t.Helper()
	ctx := context.Background()

	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ($1) RETURNING id`,
		fmt.Sprintf("test-org-%d", time.Now().UnixNano()),
	).Scan(&orgID); err != nil {
		t.Fatalf("testutil: seed org: %v", err)
	}

	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, $2, $3) RETURNING id`,
		orgID, "test-app", "testhash",
	).Scan(&appID); err != nil {
		t.Fatalf("testutil: seed app: %v", err)
	}

	return orgID, appID
}

// SeedUser inserts a user + organization_members row with role="admin"
// and returns the userID.
func SeedUser(t testing.TB, pool *pgxpool.Pool, orgID int64) int64 {
	t.Helper()
	ctx := context.Background()

	var userID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, name) VALUES ($1, $2, $3) RETURNING id`,
		fmt.Sprintf("test-%d@example.com", time.Now().UnixNano()),
		"$2a$10$placeholder", // not used in integration tests
		"Test User",
	).Scan(&userID); err != nil {
		t.Fatalf("testutil: seed user: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO organization_members (org_id, user_id, role) VALUES ($1, $2, 'admin')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("testutil: seed org member: %v", err)
	}

	return userID
}
