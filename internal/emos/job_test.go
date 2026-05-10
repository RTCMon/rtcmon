package emos_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RTCMon/rtcmon/internal/emos"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("TEST_DB_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	applyMigrations(t, pool)
	return pool
}

func applyMigrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	files := []string{
		"../../migrations/000001_initial_schema.up.sql",
		"../../migrations/000002_add_external_id.up.sql",
		"../../migrations/000003_server_api_key.up.sql",
		"../../migrations/000004_connection_stats_source.up.sql",
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			t.Fatalf("read migration %s: %v", f, err)
		}
		if _, err := pool.Exec(context.Background(), string(data)); err != nil {
			t.Logf("migration %s: %v (may be harmless)", f, err)
		}
	}
}

// setupConference inserts org → app → conference and returns all three IDs.
func setupConference(t *testing.T, pool *pgxpool.Pool) (orgID, appID, confID int64) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('emos-org') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'emos-app', 'hash') RETURNING id`,
		orgID,
	).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO conferences (app_id, external_id, started_at) VALUES ($1, 'emos-conf', now()) RETURNING id`,
		appID,
	).Scan(&confID); err != nil {
		t.Fatalf("insert conference: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, orgID)
	})
	return
}

// insertSession inserts participant → session and returns both IDs.
func insertSession(t *testing.T, pool *pgxpool.Pool, confID int64, userID string) (partID, sessID int64) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO participants (conference_id, user_id) VALUES ($1, $2) RETURNING id`,
		confID, userID,
	).Scan(&partID); err != nil {
		t.Fatalf("insert participant: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO sessions (participant_id) VALUES ($1) RETURNING id`,
		partID,
	).Scan(&sessID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return
}

// insertConnection inserts a connection row and returns its ID.
func insertConnection(t *testing.T, pool *pgxpool.Pool, sessID int64) int64 {
	t.Helper()
	var connID int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO connections (session_id) VALUES ($1) RETURNING id`,
		sessID,
	).Scan(&connID); err != nil {
		t.Fatalf("insert connection: %v", err)
	}
	return connID
}

// insertStat inserts a connection_stats row.
func insertStat(t *testing.T, pool *pgxpool.Pool, connID int64, lossRate, rttMs float32) {
	t.Helper()
	ts := time.Now().UTC()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO connection_stats (connection_id, ts, packets_lost_rate, rtt_ms)
		 VALUES ($1, $2, $3, $4)`,
		connID, ts, lossRate, rttMs,
	); err != nil {
		t.Fatalf("insert connection_stats: %v", err)
	}
}

// ─── integration tests ────────────────────────────────────────────────────────

func TestEMOSJob_WritesSessionQuality(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	_, _, confID := setupConference(t, pool)
	_, sessID := insertSession(t, pool, confID, "user-1")
	connID := insertConnection(t, pool, sessID)

	// Insert stats: 0% loss, 20ms RTT → expected eMOS ≈ 4.4.
	insertStat(t, pool, connID, 0, 20)
	insertStat(t, pool, connID, 0, 22)
	insertStat(t, pool, connID, 0, 18)

	if err := emos.RunJob(ctx, pool, confID, emos.DefaultLossCoeff, nil); err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	var e float32
	var outcome string
	if err := pool.QueryRow(ctx,
		`SELECT emos, outcome FROM session_quality WHERE session_id = $1`, sessID,
	).Scan(&e, &outcome); err != nil {
		t.Fatalf("query session_quality: %v", err)
	}

	if e < 4.0 || e > 5.0 {
		t.Errorf("expected eMOS ∈ [4.0, 5.0] for zero-loss/low-RTT, got %v", e)
	}
	if outcome != "success" {
		t.Errorf("expected outcome 'success', got %q", outcome)
	}
}

func TestEMOSJob_NoStats_Skipped(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	_, _, confID := setupConference(t, pool)
	_, sessID := insertSession(t, pool, confID, "user-2")
	// No connection or stats inserted.

	if err := emos.RunJob(ctx, pool, confID, emos.DefaultLossCoeff, nil); err != nil {
		t.Fatalf("RunJob returned error: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM session_quality WHERE session_id = $1`, sessID,
	).Scan(&count); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 0 {
		t.Errorf("expected no session_quality row for session with no stats, got %d", count)
	}
}
