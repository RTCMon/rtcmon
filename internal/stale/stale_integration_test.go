//go:build integration

package stale_test

import (
	"context"
	"testing"
	"time"

	"github.com/RTCMon/rtcmon/internal/stale"
	"github.com/RTCMon/rtcmon/internal/testutil"
)

// TestStaleJob_RunOnce_Integration verifies that RunOnce correctly marks an
// idle conference as ended and leaves an active one untouched.
func TestStaleJob_RunOnce_Integration(t *testing.T) {
	pool := testutil.StartPostgres(t)
	ctx := context.Background()

	orgID, appID := testutil.SeedApp(t, pool)
	_ = orgID

	// Insert a stale conference (no stats).
	var staleConfID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO conferences (app_id, external_id) VALUES ($1, 'stale-conf') RETURNING id`,
		appID,
	).Scan(&staleConfID); err != nil {
		t.Fatalf("insert stale conference: %v", err)
	}

	// Insert an active conference with a recent stats row.
	var activeConfID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO conferences (app_id, external_id) VALUES ($1, 'active-conf') RETURNING id`,
		appID,
	).Scan(&activeConfID); err != nil {
		t.Fatalf("insert active conference: %v", err)
	}

	// Seed the active conference with participants → sessions → connections → stats.
	var partID, sessID, connID int64
	pool.QueryRow(ctx, `INSERT INTO participants (conference_id, user_id) VALUES ($1, 'u1') RETURNING id`, activeConfID).Scan(&partID)
	pool.QueryRow(ctx, `INSERT INTO sessions (participant_id, external_id) VALUES ($1, 'sess-active') RETURNING id`, partID).Scan(&sessID)
	pool.QueryRow(ctx, `INSERT INTO connections (session_id, external_id, started_at) VALUES ($1, 'conn-active', now()) RETURNING id`, sessID).Scan(&connID)
	pool.Exec(ctx, `INSERT INTO connection_stats (connection_id, ts) VALUES ($1, now())`, connID)

	var eemosFired bool
	triggerEMOS := func(confID int64) { eemosFired = confID == staleConfID }

	if err := stale.RunOnce(ctx, pool, 15*time.Minute, 7.5, triggerEMOS, nil); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Stale conference must have ended_at set.
	var staleEnded, activeEnded *time.Time
	pool.QueryRow(ctx, `SELECT ended_at FROM conferences WHERE id = $1`, staleConfID).Scan(&staleEnded)
	pool.QueryRow(ctx, `SELECT ended_at FROM conferences WHERE id = $1`, activeConfID).Scan(&activeEnded)

	if staleEnded == nil {
		t.Error("stale conference: expected ended_at to be set")
	}
	if activeEnded != nil {
		t.Error("active conference: ended_at should not be set")
	}
	if !eemosFired {
		t.Error("expected eMOS trigger to fire for stale conference")
	}
}
