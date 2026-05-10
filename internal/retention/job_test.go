package retention_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RTCMon/rtcmon/internal/retention"
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
			t.Logf("migration %s: %v (may be harmless on re-run)", f, err)
		}
	}
}

// setupApp creates org → app with the given retention_days and registers cleanup.
func setupApp(t *testing.T, pool *pgxpool.Pool, retentionDays int) (orgID, appID int64) {
	t.Helper()
	ctx := context.Background()

	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('ret-org') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash, retention_days) VALUES ($1, 'ret-app', 'hash', $2) RETURNING id`,
		orgID, retentionDays,
	).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, orgID)
	})
	return
}

// setupHierarchy creates conference → participant → session → connection for the
// given appID and returns the connection ID. The conference started_at is set to
// the provided time so retention cutoffs apply correctly.
func setupHierarchy(t *testing.T, pool *pgxpool.Pool, appID int64, startedAt time.Time) (confID, connID int64) {
	t.Helper()
	ctx := context.Background()

	if err := pool.QueryRow(ctx,
		`INSERT INTO conferences (app_id, external_id, started_at) VALUES ($1, gen_random_uuid()::text, $2) RETURNING id`,
		appID, startedAt,
	).Scan(&confID); err != nil {
		t.Fatalf("insert conference: %v", err)
	}

	var partID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO participants (conference_id, user_id) VALUES ($1, 'ret-user') RETURNING id`,
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

	if err := pool.QueryRow(ctx,
		`INSERT INTO connections (session_id) VALUES ($1) RETURNING id`,
		sessID,
	).Scan(&connID); err != nil {
		t.Fatalf("insert connection: %v", err)
	}

	return
}

// insertEvents bulk-inserts n events for connID with the given timestamp using
// generate_series for efficiency.
func insertEvents(t *testing.T, pool *pgxpool.Pool, connID int64, ts time.Time, n int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO events (connection_id, ts, event_type)
		SELECT $1, $2, 'test'
		FROM generate_series(1, $3)
	`, connID, ts, n); err != nil {
		t.Fatalf("bulk insert events: %v", err)
	}
}

// createOldPartition creates a connection_stats partition for the given year/month.
// It also registers cleanup to drop the partition if it was not consumed by the test.
func createOldPartition(t *testing.T, pool *pgxpool.Pool, year, month int) string {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("connection_stats_%04d_%02d", year, month)
	start := fmt.Sprintf("%04d-%02d-01", year, month)

	// Compute the end of the month (first day of next month).
	endTime := time.Date(year, time.Month(month)+1, 1, 0, 0, 0, 0, time.UTC)
	end := endTime.Format("2006-01-02")

	_, err := pool.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF connection_stats FOR VALUES FROM ('%s') TO ('%s')`,
		name, start, end,
	))
	if err != nil {
		t.Fatalf("create partition %s: %v", name, err)
	}

	t.Cleanup(func() {
		// If the test dropped it already, these are no-ops (errors ignored).
		_, _ = pool.Exec(context.Background(),
			fmt.Sprintf(`ALTER TABLE connection_stats DETACH PARTITION %s`, name))
		_, _ = pool.Exec(context.Background(),
			fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name))
	})
	return name
}

// partitionExists reports whether the named partition is visible in pg_class.
func partitionExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM pg_class WHERE relname = $1 AND relkind = 'r'`, name,
	).Scan(&count); err != nil {
		t.Fatalf("query pg_class: %v", err)
	}
	return count > 0
}

// countEvents returns the number of events rows for a given connection.
func countEvents(t *testing.T, pool *pgxpool.Pool, connID int64) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM events WHERE connection_id = $1`, connID,
	).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// ─── tests ────────────────────────────────────────────────────────────────────

// TestRetention_DropsOldPartition verifies that a partition from 6 months ago is
// detached and dropped when the app's retention window is 90 days.
func TestRetention_DropsOldPartition(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// 90-day retention app.
	_, _ = setupApp(t, pool, 90)

	// Create a partition for 6 months ago (~180 days) — well outside the 90-day window.
	old := time.Now().UTC().AddDate(0, -6, 0)
	partName := createOldPartition(t, pool, old.Year(), int(old.Month()))

	if err := retention.RunOnce(ctx, pool, nil); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if partitionExists(t, pool, partName) {
		t.Errorf("expected partition %s to be dropped, but it still exists", partName)
	}
}

// TestRetention_KeepsCurrentPartition verifies that the current month's partition
// is never dropped regardless of retention_days.
func TestRetention_KeepsCurrentPartition(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	_, _ = setupApp(t, pool, 90)

	// Current month's partition is created by the initial migration.
	now := time.Now().UTC()
	currentPartName := fmt.Sprintf("connection_stats_%04d_%02d", now.Year(), int(now.Month()))

	// Ensure the partition exists (it should from migrations; create it if not).
	if !partitionExists(t, pool, currentPartName) {
		createOldPartition(t, pool, now.Year(), int(now.Month()))
	}

	if err := retention.RunOnce(ctx, pool, nil); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if !partitionExists(t, pool, currentPartName) {
		t.Errorf("current month partition %s was incorrectly dropped", currentPartName)
	}
}

// TestRetention_DeletesOldEvents verifies that events older than the retention
// window are deleted while recent events are preserved.
func TestRetention_DeletesOldEvents(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	_, appID := setupApp(t, pool, 90)
	_, connID := setupHierarchy(t, pool, appID, time.Now().UTC().AddDate(0, 0, -200))

	old := time.Now().UTC().AddDate(0, 0, -100) // 100 days ago — outside 90-day window
	recent := time.Now().UTC().AddDate(0, 0, -10) // 10 days ago — inside window

	insertEvents(t, pool, connID, old, 5)
	insertEvents(t, pool, connID, recent, 5)

	total := countEvents(t, pool, connID)
	if total != 10 {
		t.Fatalf("expected 10 events before cleanup, got %d", total)
	}

	if err := retention.RunOnce(ctx, pool, nil); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	remaining := countEvents(t, pool, connID)
	if remaining != 5 {
		t.Errorf("expected 5 events after cleanup (recent only), got %d", remaining)
	}
}

// TestRetention_BatchDelete verifies that 25,000 old events are fully deleted.
// With batchSize=10,000 this requires 3 iterations (10k + 10k + 5k = 25k).
func TestRetention_BatchDelete(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	_, appID := setupApp(t, pool, 90)
	_, connID := setupHierarchy(t, pool, appID, time.Now().UTC().AddDate(0, 0, -200))

	old := time.Now().UTC().AddDate(0, 0, -100)
	insertEvents(t, pool, connID, old, 25_000)

	if before := countEvents(t, pool, connID); before != 25_000 {
		t.Fatalf("expected 25000 events before cleanup, got %d", before)
	}

	if err := retention.RunOnce(ctx, pool, nil); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if after := countEvents(t, pool, connID); after != 0 {
		t.Errorf("expected 0 events after cleanup, got %d", after)
	}
}

// TestRetention_Idempotent verifies that running the job twice produces the same
// result with no errors on the second run.
func TestRetention_Idempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	_, appID := setupApp(t, pool, 90)
	_, connID := setupHierarchy(t, pool, appID, time.Now().UTC().AddDate(0, 0, -200))

	// Old partition — 6 months ago.
	old := time.Now().UTC().AddDate(0, -6, 0)
	partName := createOldPartition(t, pool, old.Year(), int(old.Month()))

	// Old events.
	insertEvents(t, pool, connID, time.Now().UTC().AddDate(0, 0, -100), 100)

	// First run.
	if err := retention.RunOnce(ctx, pool, nil); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if partitionExists(t, pool, partName) {
		t.Error("partition should be dropped after first run")
	}
	if n := countEvents(t, pool, connID); n != 0 {
		t.Errorf("expected 0 events after first run, got %d", n)
	}

	// Second run — partition already gone, events already gone; must not error.
	if err := retention.RunOnce(ctx, pool, nil); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if n := countEvents(t, pool, connID); n != 0 {
		t.Errorf("expected 0 events after second run, got %d", n)
	}
}
