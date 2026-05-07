package migrate_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RTCMon/rtcmon/internal/migrate"
)

// testDBURL returns the integration-test DSN from the environment.
// All tests in this file are skipped when TEST_DB_URL is absent.
func testDBURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("TEST_DB_URL not set; skipping integration test")
	}
	return url
}

// connect opens a single pgx connection for schema introspection.
func connect(t *testing.T, dbURL string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// tableExists returns true when the named table exists in the public schema.
func tableExists(t *testing.T, conn *pgx.Conn, table string) bool {
	t.Helper()
	var exists bool
	err := conn.QueryRow(context.Background(),
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = $1
		)`, table).Scan(&exists)
	if err != nil {
		t.Fatalf("tableExists(%q): %v", table, err)
	}
	return exists
}

// TestMigrations_UpDown applies up, verifies tables, applies down, verifies
// tables are gone, then applies up again to leave the schema in a clean state.
func TestMigrations_UpDown(t *testing.T) {
	dbURL := testDBURL(t)
	ctx := context.Background()

	// Start clean.
	if err := migrate.Down(ctx, dbURL, 0); err != nil {
		t.Fatalf("initial Down: %v", err)
	}

	// Apply all migrations.
	if err := migrate.Up(ctx, dbURL); err != nil {
		t.Fatalf("Up: %v", err)
	}

	conn := connect(t, dbURL)

	expectedTables := []string{
		"users", "organizations", "organization_members", "invitations",
		"apps", "conferences", "participants", "sessions", "connections",
		"connection_stats", "events", "session_quality", "share_tokens",
	}
	for _, tbl := range expectedTables {
		if !tableExists(t, conn, tbl) {
			t.Errorf("table %q not found after migrate up", tbl)
		}
	}

	// Roll back all migrations.
	if err := migrate.Down(ctx, dbURL, 0); err != nil {
		t.Fatalf("Down(0): %v", err)
	}

	for _, tbl := range expectedTables {
		if tableExists(t, conn, tbl) {
			t.Errorf("table %q still exists after migrate down", tbl)
		}
	}

	// Re-apply so the DB is usable for subsequent tests.
	if err := migrate.Up(ctx, dbURL); err != nil {
		t.Fatalf("re-Up: %v", err)
	}
}

// TestMigrations_IdempotentUp verifies that running migrate up twice is safe.
func TestMigrations_IdempotentUp(t *testing.T) {
	dbURL := testDBURL(t)
	ctx := context.Background()

	if err := migrate.Up(ctx, dbURL); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	if err := migrate.Up(ctx, dbURL); err != nil {
		t.Fatalf("second Up (should be no-op): %v", err)
	}
}

// TestPartition_InsertCurrentMonth inserts a connection_stats row with ts=now()
// and verifies it lands in the current month's partition.
func TestPartition_InsertCurrentMonth(t *testing.T) {
	dbURL := testDBURL(t)
	ctx := context.Background()

	if err := migrate.Up(ctx, dbURL); err != nil {
		t.Fatalf("Up: %v", err)
	}

	conn := connect(t, dbURL)

	// Seed required parent rows.
	connID := seedConnection(t, conn)

	// Insert into partitioned table using current timestamp.
	now := time.Now().UTC()
	_, err := conn.Exec(ctx,
		`INSERT INTO connection_stats (connection_id, ts, rtt_ms) VALUES ($1, $2, $3)`,
		connID, now, 42.0,
	)
	if err != nil {
		t.Fatalf("INSERT connection_stats (current month): %v", err)
	}

	// Verify the row landed in the correct partition.
	expectedPartition := "connection_stats_" + now.Format("2006_01")
	if !tableExists(t, conn, expectedPartition) {
		t.Errorf("partition %q not found", expectedPartition)
	}

	var count int
	err = conn.QueryRow(ctx,
		`SELECT COUNT(*) FROM `+expectedPartition+` WHERE connection_id = $1`, connID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count from partition: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row in %s, got %d", expectedPartition, count)
	}
}

// TestPartition_InsertNextMonth inserts a row with ts in next month and verifies
// it lands in the next month's partition.
func TestPartition_InsertNextMonth(t *testing.T) {
	dbURL := testDBURL(t)
	ctx := context.Background()

	if err := migrate.Up(ctx, dbURL); err != nil {
		t.Fatalf("Up: %v", err)
	}

	conn := connect(t, dbURL)
	connID := seedConnection(t, conn)

	nextMonth := time.Now().UTC().AddDate(0, 1, 0)
	_, err := conn.Exec(ctx,
		`INSERT INTO connection_stats (connection_id, ts, rtt_ms) VALUES ($1, $2, $3)`,
		connID, nextMonth, 55.0,
	)
	if err != nil {
		t.Fatalf("INSERT connection_stats (next month): %v", err)
	}

	expectedPartition := "connection_stats_" + nextMonth.Format("2006_01")
	if !tableExists(t, conn, expectedPartition) {
		t.Errorf("partition %q not found", expectedPartition)
	}
}

// TestForeignKey_ConnectionRefSession verifies that inserting a connections row
// with a non-existent session_id returns a FK violation error.
func TestForeignKey_ConnectionRefSession(t *testing.T) {
	dbURL := testDBURL(t)
	ctx := context.Background()

	if err := migrate.Up(ctx, dbURL); err != nil {
		t.Fatalf("Up: %v", err)
	}

	conn := connect(t, dbURL)

	_, err := conn.Exec(ctx,
		`INSERT INTO connections (session_id) VALUES (99999999)`,
	)
	if err == nil {
		t.Fatal("expected FK violation for non-existent session_id, got nil")
	}
}

// TestSchema_EWMAColumns verifies all five ewma_* columns exist on
// connection_stats with float4 (real) type.
func TestSchema_EWMAColumns(t *testing.T) {
	dbURL := testDBURL(t)
	ctx := context.Background()

	if err := migrate.Up(ctx, dbURL); err != nil {
		t.Fatalf("Up: %v", err)
	}

	conn := connect(t, dbURL)

	ewmaCols := []string{
		"ewma_packets_lost_rate",
		"ewma_jitter_ms",
		"ewma_rtt_ms",
		"ewma_bitrate_in_kbps",
		"ewma_bitrate_out_kbps",
	}

	for _, col := range ewmaCols {
		var dataType string
		err := conn.QueryRow(ctx,
			`SELECT data_type FROM information_schema.columns
			 WHERE table_schema = 'public'
			   AND table_name   = 'connection_stats'
			   AND column_name  = $1`,
			col,
		).Scan(&dataType)
		if err != nil {
			t.Errorf("column %q not found in connection_stats: %v", col, err)
			continue
		}
		if dataType != "real" {
			t.Errorf("column %q: expected type 'real', got %q", col, dataType)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Seed helpers
// ─────────────────────────────────────────────────────────────────────────────

// seedConnection inserts the minimal hierarchy (org → app → conference →
// participant → session → connection) and returns the connection ID.
func seedConnection(t *testing.T, conn *pgx.Conn) int64 {
	t.Helper()
	ctx := context.Background()

	var orgID int64
	if err := conn.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('test-org') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}

	var appID int64
	if err := conn.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'test-app', 'hash') RETURNING id`,
		orgID,
	).Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	var confID int64
	if err := conn.QueryRow(ctx,
		`INSERT INTO conferences (app_id, external_id) VALUES ($1, 'ext-1') RETURNING id`,
		appID,
	).Scan(&confID); err != nil {
		t.Fatalf("seed conference: %v", err)
	}

	var partID int64
	if err := conn.QueryRow(ctx,
		`INSERT INTO participants (conference_id, user_id) VALUES ($1, 'u1') RETURNING id`,
		confID,
	).Scan(&partID); err != nil {
		t.Fatalf("seed participant: %v", err)
	}

	var sessID int64
	if err := conn.QueryRow(ctx,
		`INSERT INTO sessions (participant_id) VALUES ($1) RETURNING id`,
		partID,
	).Scan(&sessID); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	var connID int64
	if err := conn.QueryRow(ctx,
		`INSERT INTO connections (session_id) VALUES ($1) RETURNING id`,
		sessID,
	).Scan(&connID); err != nil {
		t.Fatalf("seed connection: %v", err)
	}

	return connID
}
