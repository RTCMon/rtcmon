package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/auth"
	"github.com/RTCMon/rtcmon/internal/ingest"
	"github.com/RTCMon/rtcmon/internal/model"
	"github.com/RTCMon/rtcmon/internal/ratelimit"
	"github.com/RTCMon/rtcmon/internal/worker"
	"github.com/RTCMon/rtcmon/services/ingest-api/server"
)

const e2eSecret = "e2e-test-secret"

// ── Infrastructure helpers ───────────────────────────────────────────────────

func e2eDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("TEST_DB_URL not set; skipping e2e test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("e2e db connect: %v", err)
	}
	t.Cleanup(pool.Close)
	runE2EMigrations(t, pool)
	return pool
}

func runE2EMigrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, f := range []string{
		"../../../migrations/000001_initial_schema.up.sql",
		"../../../migrations/000002_add_external_id.up.sql",
	} {
		data, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			t.Fatalf("read migration %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			t.Logf("migration %s: %v (may be harmless on existing schema)", f, err)
		}
	}
}

func e2eRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func discardLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

// newE2EServer wires the complete ingest stack and returns an httptest.Server.
// The worker pool uses a 50ms flush interval so DB rows appear quickly in tests.
func newE2EServer(t *testing.T) *httptest.Server {
	t.Helper()
	pool := e2eDB(t)
	rdb := e2eRedis(t)
	log := discardLogger()

	flusher := ingest.New(pool, rdb, log, t.TempDir())
	wp := worker.New(worker.Config{
		WorkerCount:     2,
		BatchSize:       50,
		FlushIntervalMs: 50,
		ChannelCap:      1000,
	}, flusher.Flush, log)
	t.Cleanup(wp.Shutdown)

	rl := ratelimit.New(rdb, ratelimit.Config{Max: 10000, WindowSecs: 60}, log)
	srv := server.NewServer(context.Background(), pool, rdb, log, e2eSecret, rl, wp.Enqueue)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// seedE2EApp inserts the minimal org + app rows and returns the numeric app ID.
func seedE2EApp(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()

	var orgID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('e2e-org') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}

	var appID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'e2e-app', 'hash') RETURNING id`,
		orgID,
	).Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	return appID
}

// makeE2EToken signs a JWT with the given appID string.
func makeE2EToken(t *testing.T, appID string) string {
	t.Helper()
	claims := auth.Claims{}
	claims.AppID = appID
	claims.ConferenceID = "conf-e2e"
	claims.UserID = "user-e2e"
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Hour))
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(e2eSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}

func postEvents(t *testing.T, ts *httptest.Server, token string, payload model.IngestPayload) *http.Response {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/events", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func validE2EPayload() model.IngestPayload {
	return model.IngestPayload{
		ConferenceID: "conf-e2e",
		SessionID:    "sess-e2e",
		ConnectionID: "conn-e2e",
		Events:       []model.StatSnapshot{{TS: time.Now().UnixMilli(), RTTMs: 10}},
	}
}

// ── Tests ────────────────────────────────────────────────────────────────────

// TestIngest_E2E_Valid sends a valid authenticated request and verifies that a
// connection_stats row appears in the DB within 200ms.
func TestIngest_E2E_Valid(t *testing.T) {
	ts := newE2EServer(t)
	pool := e2eDB(t)
	appID := seedE2EApp(t, pool)
	token := makeE2EToken(t, fmt.Sprintf("%d", appID))

	resp := postEvents(t, ts, token, validE2EPayload())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	ctx := context.Background()
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		var count int
		pool.QueryRow(ctx, `SELECT COUNT(*) FROM connection_stats`).Scan(&count)
		if count > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("connection_stats row not found within 200ms")
}

// TestIngest_E2E_ResponseTime verifies that the handler returns 202 in < 5ms
// (enqueue is non-blocking; DB write happens asynchronously).
func TestIngest_E2E_ResponseTime(t *testing.T) {
	ts := newE2EServer(t)
	pool := e2eDB(t)
	appID := seedE2EApp(t, pool)
	token := makeE2EToken(t, fmt.Sprintf("%d", appID))

	start := time.Now()
	resp := postEvents(t, ts, token, validE2EPayload())
	elapsed := time.Since(start)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	if elapsed > 5*time.Millisecond {
		t.Errorf("want response < 5ms, got %v", elapsed)
	}
}

// TestIngest_E2E_ChannelFull verifies that a full worker channel returns 429
// with Retry-After: 1. Uses a stub enqueue that always returns ErrChannelFull.
func TestIngest_E2E_ChannelFull(t *testing.T) {
	// e2eDB just for the skip check — we don't need real DB for this test.
	pool := e2eDB(t)
	rdb := e2eRedis(t)
	log := discardLogger()

	alwaysFull := func(model.IngestPayload) error { return worker.ErrChannelFull }
	rl := ratelimit.New(rdb, ratelimit.Config{Max: 10000, WindowSecs: 60}, log)
	srv := server.NewServer(context.Background(), pool, rdb, log, e2eSecret, rl, alwaysFull)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	appID := seedE2EApp(t, pool)
	token := makeE2EToken(t, fmt.Sprintf("%d", appID))

	resp := postEvents(t, ts, token, validE2EPayload())
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("want 429, got %d", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "1" {
		t.Errorf("want Retry-After: 1, got %q", ra)
	}
}

// TestIngest_E2E_UnauthorizedSkipsChannel verifies that requests without a
// valid JWT are rejected with 401 before touching the worker channel.
func TestIngest_E2E_UnauthorizedSkipsChannel(t *testing.T) {
	pool := e2eDB(t)
	rdb := e2eRedis(t)
	log := discardLogger()

	var enqueueCount int
	countingEnqueue := func(model.IngestPayload) error {
		enqueueCount++
		return nil
	}
	rl := ratelimit.New(rdb, ratelimit.Config{Max: 10000, WindowSecs: 60}, log)
	srv := server.NewServer(context.Background(), pool, rdb, log, e2eSecret, rl, countingEnqueue)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	// No Authorization header.
	resp := postEvents(t, ts, "", validE2EPayload())
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
	if enqueueCount != 0 {
		t.Errorf("want 0 enqueue calls, got %d", enqueueCount)
	}
}

// TestIngest_E2E_AppIDPropagated verifies that the app_id stored in
// connection_stats (via the conferences FK chain) matches the AppID from the JWT.
func TestIngest_E2E_AppIDPropagated(t *testing.T) {
	ts := newE2EServer(t)
	pool := e2eDB(t)
	appID := seedE2EApp(t, pool)
	token := makeE2EToken(t, fmt.Sprintf("%d", appID))

	resp := postEvents(t, ts, token, validE2EPayload())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	// Poll until the row exists, then verify the app_id chain.
	ctx := context.Background()
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		var gotAppID int64
		err := pool.QueryRow(ctx, `
			SELECT conf.app_id
			FROM connection_stats cs
			JOIN connections conn ON conn.id = cs.connection_id
			JOIN sessions sess     ON sess.id = conn.session_id
			JOIN participants part ON part.id = sess.participant_id
			JOIN conferences conf  ON conf.id = part.conference_id
			LIMIT 1
		`).Scan(&gotAppID)
		if err == nil {
			if gotAppID != appID {
				t.Errorf("want app_id=%d, got %d", appID, gotAppID)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("no connection_stats row found within 200ms")
}

// TestIngest_E2E_100ConcurrentRequests sends 100 authenticated POST requests
// concurrently and verifies that all receive 202 and all rows eventually
// appear in the DB after the worker pool drains.
func TestIngest_E2E_100ConcurrentRequests(t *testing.T) {
	ts := newE2EServer(t)
	pool := e2eDB(t)
	appID := seedE2EApp(t, pool)
	token := makeE2EToken(t, fmt.Sprintf("%d", appID))

	const n = 100
	var wg sync.WaitGroup
	codes := make([]int, n)

	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			p := model.IngestPayload{
				ConferenceID: "conf-concurrent",
				SessionID:    fmt.Sprintf("sess-%d", idx),
				ConnectionID: fmt.Sprintf("conn-%d", idx),
				Events:       []model.StatSnapshot{{TS: time.Now().UnixMilli() + int64(idx), RTTMs: float64(idx)}},
			}
			resp := postEvents(t, ts, token, p)
			codes[idx] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusAccepted {
			t.Errorf("goroutine %d: want 202, got %d", i, code)
		}
	}

	// Close the test server to trigger wp.Shutdown via t.Cleanup, then poll.
	ts.Close()

	ctx := context.Background()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		var count int
		pool.QueryRow(ctx, `SELECT COUNT(*) FROM connection_stats`).Scan(&count)
		if count == n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	var final int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM connection_stats`).Scan(&final)
	if final != n {
		t.Errorf("want %d rows in connection_stats, got %d", n, final)
	}
}
