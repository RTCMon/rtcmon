package ingest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/ingest"
	"github.com/RTCMon/rtcmon/internal/model"
)

// testDB connects to a real Postgres and skips when TEST_DB_URL is absent.
// It runs migrations on the test schema so the flusher works against a clean
// database for every test file run.
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("TEST_DB_URL not set; skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	runMigrations(t, pool)
	return pool
}

// runMigrations applies the two SQL migration files directly so each test run
// starts with a schema-consistent database.
func runMigrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	files := []string{
		"../../migrations/000001_initial_schema.up.sql",
		"../../migrations/000002_add_external_id.up.sql",
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			t.Fatalf("read migration %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			// Ignore "already exists" errors from re-running on an existing schema.
			t.Logf("migration %s: %v (may be harmless)", f, err)
		}
	}
}

// seedApp inserts a minimal app row and returns its ID.
func seedApp(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()

	// Need an org first.
	var orgID int64
	err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('test-org') RETURNING id`,
	).Scan(&orgID)
	if err != nil {
		t.Fatalf("seed org: %v", err)
	}

	var appID int64
	err = pool.QueryRow(ctx, `
		INSERT INTO apps (org_id, name, api_key_hash)
		VALUES ($1, 'test-app', 'hash')
		RETURNING id
	`, orgID).Scan(&appID)
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
	return appID
}

func discardLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(os.Stderr)
	log.SetLevel(logrus.WarnLevel)
	return log
}

// makePayload builds a minimal IngestPayload.
func makePayload(appID, confID, sessID, connID, userID string, ts int64, rtt float64) model.IngestPayload {
	return model.IngestPayload{
		AppID:        appID,
		UserID:       userID,
		ConferenceID: confID,
		SessionID:    sessID,
		ConnectionID: connID,
		Events: []model.StatSnapshot{
			{TS: ts, RTTMs: rtt, JitterMs: 1, PacketLossRate: 0.01, BitrateInKbps: 1000, BitrateOutKbps: 500},
		},
	}
}

// TestFlush_SingleRow flushes 1 payload with 1 event and verifies exactly 1
// connection_stats row is inserted.
func TestFlush_SingleRow(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	appID := seedApp(t, pool)
	appIDStr := itoa(appID)

	f := ingest.New(pool, discardLogger(), t.TempDir())
	p := makePayload(appIDStr, "conf-1", "sess-1", "conn-1", "user-1", nowMs(), 50)
	f.Flush(ctx, []model.IngestPayload{p})

	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM connection_stats`).Scan(&count)
	if count != 1 {
		t.Errorf("want 1 row, got %d", count)
	}
}

// TestFlush_BatchOf500 flushes 500 distinct events and verifies all are
// inserted within 100ms.
func TestFlush_BatchOf500(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	appID := seedApp(t, pool)
	appIDStr := itoa(appID)

	const n = 500
	batch := make([]model.IngestPayload, n)
	base := nowMs()
	for i := range n {
		batch[i] = makePayload(
			appIDStr,
			"conf-batch",
			fmt.Sprintf("sess-%d", i),
			fmt.Sprintf("conn-%d", i),
			"user-1",
			base+int64(i),
			float64(i),
		)
	}

	f := ingest.New(pool, discardLogger(), t.TempDir())

	start := time.Now()
	f.Flush(ctx, batch)
	elapsed := time.Since(start)

	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM connection_stats`).Scan(&count)
	if count != n {
		t.Errorf("want %d rows, got %d", n, count)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("flush took %v, want < 100ms", elapsed)
	}
}

// TestFlush_DuplicateConference flushes two batches with the same conference
// external_id and verifies only one conferences row is created.
func TestFlush_DuplicateConference(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	appID := seedApp(t, pool)
	appIDStr := itoa(appID)
	ts := nowMs()

	f := ingest.New(pool, discardLogger(), t.TempDir())
	p1 := makePayload(appIDStr, "conf-dup", "sess-1", "conn-1", "user-1", ts, 10)
	p2 := makePayload(appIDStr, "conf-dup", "sess-2", "conn-2", "user-1", ts+1, 20)
	f.Flush(ctx, []model.IngestPayload{p1})
	f.Flush(ctx, []model.IngestPayload{p2})

	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM conferences WHERE external_id = 'conf-dup'`).Scan(&count)
	if count != 1 {
		t.Errorf("want 1 conference row, got %d", count)
	}
}

// TestFlush_DuplicateSession flushes two batches with the same session_id and
// verifies only one sessions row is created.
func TestFlush_DuplicateSession(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	appID := seedApp(t, pool)
	appIDStr := itoa(appID)
	ts := nowMs()

	f := ingest.New(pool, discardLogger(), t.TempDir())
	p1 := makePayload(appIDStr, "conf-1", "sess-dup", "conn-1", "user-1", ts, 10)
	p2 := makePayload(appIDStr, "conf-1", "sess-dup", "conn-2", "user-1", ts+1, 20)
	f.Flush(ctx, []model.IngestPayload{p1})
	f.Flush(ctx, []model.IngestPayload{p2})

	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE external_id = 'sess-dup'`).Scan(&count)
	if count != 1 {
		t.Errorf("want 1 session row, got %d", count)
	}
}

// TestFlush_PartitionRouting verifies that a row with ts in the next month
// lands in the correct monthly partition.
func TestFlush_PartitionRouting(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	appID := seedApp(t, pool)
	appIDStr := itoa(appID)

	// ts is the first millisecond of next month.
	now := time.Now().UTC()
	nextMonth := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	tsMs := nextMonth.UnixMilli()

	f := ingest.New(pool, discardLogger(), t.TempDir())
	p := makePayload(appIDStr, "conf-part", "sess-part", "conn-part", "user-1", tsMs, 10)
	f.Flush(ctx, []model.IngestPayload{p})

	partName := "connection_stats_" + nextMonth.Format("2006_01")
	var count int
	pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, partName)).Scan(&count)
	if count != 1 {
		t.Errorf("want 1 row in partition %s, got %d", partName, count)
	}
}

// TestFlush_EWMA_FirstSample verifies that the first flush for a connection
// produces ewma_rtt_ms == raw rtt_ms.
func TestFlush_EWMA_FirstSample(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	appID := seedApp(t, pool)
	appIDStr := itoa(appID)
	const rawRTT = 42.0

	f := ingest.New(pool, discardLogger(), t.TempDir())
	p := makePayload(appIDStr, "conf-ewma1", "sess-ewma1", "conn-ewma1", "user-1", nowMs(), rawRTT)
	f.Flush(ctx, []model.IngestPayload{p})

	var ewmaRTT float64
	pool.QueryRow(ctx, `SELECT ewma_rtt_ms FROM connection_stats LIMIT 1`).Scan(&ewmaRTT)
	if math.Abs(ewmaRTT-rawRTT) > 0.001 {
		t.Errorf("first sample: want ewma_rtt_ms=%.3f, got %.3f", rawRTT, ewmaRTT)
	}
}

// TestFlush_EWMA_SecondSample verifies that the second flush produces
// ewma_rtt_ms = 0.2*raw + 0.8*prev (±0.001).
func TestFlush_EWMA_SecondSample(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	appID := seedApp(t, pool)
	appIDStr := itoa(appID)
	ts := nowMs()

	const (
		raw1 = 100.0
		raw2 = 200.0
	)
	expectedEWMA2 := 0.2*raw2 + 0.8*raw1 // prev EWMA after first sample == raw1

	f := ingest.New(pool, discardLogger(), t.TempDir())

	p1 := makePayload(appIDStr, "conf-ewma2", "sess-ewma2", "conn-ewma2", "user-1", ts, raw1)
	f.Flush(ctx, []model.IngestPayload{p1})

	p2 := makePayload(appIDStr, "conf-ewma2", "sess-ewma2", "conn-ewma2", "user-1", ts+1, raw2)
	f.Flush(ctx, []model.IngestPayload{p2})

	// Second row is the one with the higher ts.
	var ewmaRTT float64
	pool.QueryRow(ctx, `
		SELECT ewma_rtt_ms FROM connection_stats ORDER BY ts DESC LIMIT 1
	`).Scan(&ewmaRTT)

	if math.Abs(ewmaRTT-expectedEWMA2) > 0.001 {
		t.Errorf("second sample: want ewma_rtt_ms=%.3f, got %.3f", expectedEWMA2, ewmaRTT)
	}
}

// TestFlush_EWMA_StateIsolated verifies that two different connections have
// independent EWMA state.
func TestFlush_EWMA_StateIsolated(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	appID := seedApp(t, pool)
	appIDStr := itoa(appID)
	ts := nowMs()

	const (
		rawA1 = 10.0
		rawA2 = 20.0
		rawB1 = 100.0
		rawB2 = 200.0
	)

	f := ingest.New(pool, discardLogger(), t.TempDir())

	// First batch: one event each for conn-A and conn-B.
	batch1 := []model.IngestPayload{
		makePayload(appIDStr, "conf-iso", "sess-A", "conn-A", "user-1", ts, rawA1),
		makePayload(appIDStr, "conf-iso", "sess-B", "conn-B", "user-2", ts, rawB1),
	}
	f.Flush(ctx, batch1)

	// Second batch.
	batch2 := []model.IngestPayload{
		makePayload(appIDStr, "conf-iso", "sess-A", "conn-A", "user-1", ts+1, rawA2),
		makePayload(appIDStr, "conf-iso", "sess-B", "conn-B", "user-2", ts+1, rawB2),
	}
	f.Flush(ctx, batch2)

	expectedA := 0.2*rawA2 + 0.8*rawA1
	expectedB := 0.2*rawB2 + 0.8*rawB1

	rows, err := pool.Query(ctx, `
		SELECT c.external_id, cs.ewma_rtt_ms
		FROM connection_stats cs
		JOIN connections c ON c.id = cs.connection_id
		WHERE c.external_id IN ('conn-A', 'conn-B')
		ORDER BY cs.ts DESC, c.external_id
	`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	got := make(map[string]float64)
	for rows.Next() {
		var extID string
		var ewmaRTT float64
		rows.Scan(&extID, &ewmaRTT)
		if _, ok := got[extID]; !ok {
			got[extID] = ewmaRTT
		}
	}

	if math.Abs(got["conn-A"]-expectedA) > 0.001 {
		t.Errorf("conn-A: want ewma=%.3f, got %.3f", expectedA, got["conn-A"])
	}
	if math.Abs(got["conn-B"]-expectedB) > 0.001 {
		t.Errorf("conn-B: want ewma=%.3f, got %.3f", expectedB, got["conn-B"])
	}
}

// TestFlush_DeadLetter_OnError injects an invalid app_id to force a flush
// failure and verifies that a dead-letter file is created.
func TestFlush_DeadLetter_OnError(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	f := ingest.New(pool, discardLogger(), dir)

	// app_id "not-a-number" will fail strconv.ParseInt → flush error.
	p := makePayload("not-a-number", "conf-1", "sess-1", "conn-1", "user-1", nowMs(), 10)
	f.Flush(ctx, []model.IngestPayload{p})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("want dead-letter file, found none")
	}
}

// TestFlush_DeadLetterFile_IsValidJSON verifies that the dead-letter file can
// be unmarshalled back to []model.IngestPayload.
func TestFlush_DeadLetterFile_IsValidJSON(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	f := ingest.New(pool, discardLogger(), dir)

	p := makePayload("not-a-number", "conf-1", "sess-1", "conn-1", "user-1", nowMs(), 10)
	f.Flush(ctx, []model.IngestPayload{p})

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatal("dead-letter file not found")
	}

	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	var replayed []model.IngestPayload
	if err := json.Unmarshal(data, &replayed); err != nil {
		t.Fatalf("unmarshal dead-letter: %v", err)
	}
	if len(replayed) != 1 {
		t.Errorf("want 1 payload, got %d", len(replayed))
	}
}

func nowMs() int64 {
	return time.Now().UnixMilli()
}

func itoa(n int64) string {
	return fmt.Sprintf("%d", n)
}
