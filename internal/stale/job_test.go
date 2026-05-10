package stale_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/goleak"

	"github.com/RTCMon/rtcmon/internal/emos"
	"github.com/RTCMon/rtcmon/internal/stale"
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

// setupConference inserts org → app → conference and registers cleanup.
func setupConference(t *testing.T, pool *pgxpool.Pool) (orgID, appID, confID int64) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('stale-org') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'stale-app', 'hash') RETURNING id`,
		orgID,
	).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO conferences (app_id, external_id, started_at) VALUES ($1, 'stale-conf', now()) RETURNING id`,
		appID,
	).Scan(&confID); err != nil {
		t.Fatalf("insert conference: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, orgID)
	})
	return
}

// insertStatAt inserts a connection_stats row at a specific timestamp via the
// full hierarchy: participant → session → connection → connection_stats.
func insertStatAt(t *testing.T, pool *pgxpool.Pool, confID int64, ts time.Time) {
	t.Helper()
	ctx := context.Background()

	var partID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO participants (conference_id, user_id) VALUES ($1, 'stat-user') RETURNING id
		 ON CONFLICT (conference_id, user_id) DO UPDATE SET user_id = EXCLUDED.user_id RETURNING id`,
		confID,
	).Scan(&partID); err != nil {
		t.Fatalf("insert participant: %v", err)
	}

	var sessID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO sessions (participant_id) VALUES ($1) RETURNING id`,
		partID,
	).Scan(&sessID); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	var connID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO connections (session_id) VALUES ($1) RETURNING id`,
		sessID,
	).Scan(&connID); err != nil {
		t.Fatalf("insert connection: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO connection_stats (connection_id, ts, packets_lost_rate, rtt_ms) VALUES ($1, $2, 0, 20)`,
		connID, ts,
	); err != nil {
		t.Fatalf("insert connection_stats: %v", err)
	}
}

// ─── unit test (no DB) ───────────────────────────────────────────────────────

func TestStaleJob_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	// Job with nil db — we only test Start/Shutdown lifecycle, not RunOnce.
	j := stale.New(nil, nil, time.Hour, 15*time.Minute, emos.DefaultLossCoeff, nil)
	j.Start()
	j.Shutdown() // must block until goroutine exits
}

// ─── integration tests (require TEST_DB_URL) ─────────────────────────────────

func TestStaleJob_MarksIdleConference(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	_, _, confID := setupConference(t, pool)
	// Insert a stat 20 minutes ago → conference is stale (threshold 15m).
	insertStatAt(t, pool, confID, time.Now().UTC().Add(-20*time.Minute))

	var triggered []int64
	trigger := func(id int64) { triggered = append(triggered, id) }

	if err := stale.RunOnce(ctx, pool, 15*time.Minute, emos.DefaultLossCoeff, trigger, nil); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// ended_at must be set.
	var endedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT ended_at FROM conferences WHERE id = $1`, confID,
	).Scan(&endedAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if endedAt == nil {
		t.Error("expected ended_at to be set, got NULL")
	}

	// eMOS trigger must have been called.
	if len(triggered) == 0 {
		t.Error("expected eMOS trigger to be called")
	}
}

func TestStaleJob_IgnoresActiveConference(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	_, _, confID := setupConference(t, pool)
	// Insert a stat 5 minutes ago → conference is still active (threshold 15m).
	insertStatAt(t, pool, confID, time.Now().UTC().Add(-5*time.Minute))

	if err := stale.RunOnce(ctx, pool, 15*time.Minute, emos.DefaultLossCoeff, nil, nil); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	var endedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT ended_at FROM conferences WHERE id = $1`, confID,
	).Scan(&endedAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if endedAt != nil {
		t.Errorf("active conference must not be closed, got ended_at=%v", endedAt)
	}
}

func TestStaleJob_Idempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	_, _, confID := setupConference(t, pool)
	insertStatAt(t, pool, confID, time.Now().UTC().Add(-20*time.Minute))

	// Run the eMOS job directly (simulating what the trigger would do) after
	// the first stale scan so session_quality rows exist before the second run.
	trigger := func(id int64) {
		_ = emos.RunJob(ctx, pool, id, emos.DefaultLossCoeff, nil)
	}

	// First run: closes the conference and writes session_quality.
	if err := stale.RunOnce(ctx, pool, 15*time.Minute, emos.DefaultLossCoeff, trigger, nil); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}

	var countAfterFirst int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM session_quality sq
		 JOIN sessions s ON s.id = sq.session_id
		 JOIN participants p ON p.id = s.participant_id
		 WHERE p.conference_id = $1`, confID,
	).Scan(&countAfterFirst); err != nil {
		t.Fatalf("query count: %v", err)
	}

	// Second run: conference already has ended_at set → no new triggers, count unchanged.
	if err := stale.RunOnce(ctx, pool, 15*time.Minute, emos.DefaultLossCoeff, trigger, nil); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}

	var countAfterSecond int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM session_quality sq
		 JOIN sessions s ON s.id = sq.session_id
		 JOIN participants p ON p.id = s.participant_id
		 WHERE p.conference_id = $1`, confID,
	).Scan(&countAfterSecond); err != nil {
		t.Fatalf("query count: %v", err)
	}

	if countAfterSecond != countAfterFirst {
		t.Errorf("idempotent: session_quality count changed from %d to %d on second run",
			countAfterFirst, countAfterSecond)
	}
}
