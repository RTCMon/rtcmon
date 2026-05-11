//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/emos"
	"github.com/RTCMon/rtcmon/internal/ingest"
	"github.com/RTCMon/rtcmon/internal/model"
	"github.com/RTCMon/rtcmon/internal/ratelimit"
	"github.com/RTCMon/rtcmon/internal/session"
	"github.com/RTCMon/rtcmon/internal/testutil"
	"github.com/RTCMon/rtcmon/internal/worker"
	ingestserver "github.com/RTCMon/rtcmon/services/ingest-api/server"
	queryhandler "github.com/RTCMon/rtcmon/services/query-api/handler"
)

const testJWTSecret = "integration-test-secret"

// makeJWT signs a minimal SDK JWT with the given claims.
func makeJWT(t testing.TB, appID, confID, userID string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"app_id":        appID,
		"conference_id": confID,
		"user_id":       userID,
		"exp":           time.Now().Add(time.Hour).Unix(),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("makeJWT: %v", err)
	}
	return tok
}

// noopLog returns a silent logger suitable for tests.
func noopLog() *logrus.Logger {
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)
	return log
}

// withSession wraps r with session data injected into the context.
func withSession(r *http.Request, data *session.Data) *http.Request {
	return r.WithContext(session.WithContext(r.Context(), data))
}

// withChiParam injects chi URL params into the request context.
func withChiParam(r *http.Request, params map[string]string) *http.Request {
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// samplePayload returns an IngestPayload wired to appID with realistic stats.
func samplePayload(appID, userID, confID, sessID, connID string, rttMs, lossRate float64) model.IngestPayload {
	now := time.Now().UTC().UnixMilli()
	return model.IngestPayload{
		AppID:        appID,
		UserID:       userID,
		ConferenceID: confID,
		SessionID:    sessID,
		ConnectionID: connID,
		Events: []model.StatSnapshot{
			{
				TS:             now,
				RTTMs:          rttMs,
				JitterMs:       5,
				PacketLossRate: lossRate,
				BitrateInKbps:  500,
				BitrateOutKbps: 300,
			},
			{
				TS:             now + 1000,
				RTTMs:          rttMs,
				JitterMs:       6,
				PacketLossRate: lossRate,
				BitrateInKbps:  510,
				BitrateOutKbps: 310,
			},
		},
	}
}

// ─── Test: full ingest-to-query data path ────────────────────────────────────

// TestIntegration_IngestToQuery flushes synthetic data via the Flusher and
// verifies three query-api endpoints return consistent results.
func TestIntegration_IngestToQuery(t *testing.T) {
	pool := testutil.StartPostgres(t)
	rdb := testutil.StartRedis(t)
	orgID, appID := testutil.SeedApp(t, pool)
	appIDStr := strconv.FormatInt(appID, 10)

	flusher := ingest.New(pool, rdb, noopLog(), t.TempDir())
	payload := samplePayload(appIDStr, "user-1", "conf-abc", "sess-1", "conn-1", 20, 0.0)
	flusher.Flush(context.Background(), []model.IngestPayload{payload})

	sess := &session.Data{OrgID: orgID, UserID: 1, Role: "admin"}
	log := noopLog()

	// ── conference list ──────────────────────────────────────────────────────
	t.Run("conference list", func(t *testing.T) {
		from := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
		to := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
		url := fmt.Sprintf("/v1/apps/%d/conferences?from=%s&to=%s", appID, from, to)
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req = withChiParam(req, map[string]string{"appId": appIDStr})
		req = withSession(req, sess)

		w := httptest.NewRecorder()
		queryhandler.HandleListConferences(pool, log)(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("conference list: want 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp queryhandler.ConferenceListResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("conference list: decode: %v", err)
		}
		if resp.Pagination.Total < 1 {
			t.Fatalf("conference list: want ≥1 conference, got %d", resp.Pagination.Total)
		}
	})

	// ── conference detail ────────────────────────────────────────────────────
	t.Run("conference detail", func(t *testing.T) {
		// Look up the conference DB id to test the detail endpoint.
		var confDBID int64
		err := pool.QueryRow(context.Background(),
			`SELECT id FROM conferences WHERE app_id = $1 LIMIT 1`, appID,
		).Scan(&confDBID)
		if err != nil {
			t.Fatalf("conference detail: lookup id: %v", err)
		}

		url := fmt.Sprintf("/v1/conferences/%d", confDBID)
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req = withChiParam(req, map[string]string{"conferenceId": strconv.FormatInt(confDBID, 10)})
		req = withSession(req, sess)

		w := httptest.NewRecorder()
		queryhandler.HandleGetConference(pool, log)(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("conference detail: want 200, got %d: %s", w.Code, w.Body.String())
		}
		var body map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("conference detail: decode: %v", err)
		}
		if body["id"] == nil {
			t.Fatal("conference detail: response missing id field")
		}
	})

	// ── analytics overview ───────────────────────────────────────────────────
	t.Run("analytics overview", func(t *testing.T) {
		from := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
		to := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
		url := fmt.Sprintf("/v1/apps/%d/analytics/overview?from=%s&to=%s", appID, from, to)
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req = withChiParam(req, map[string]string{"appId": appIDStr})
		req = withSession(req, sess)

		w := httptest.NewRecorder()
		queryhandler.HandleGetAnalyticsOverview(pool, rdb, log)(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("analytics overview: want 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp queryhandler.OverviewResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("analytics overview: decode: %v", err)
		}
		if resp.TotalConferences < 1 {
			t.Fatalf("analytics overview: want total_conferences ≥1, got %d", resp.TotalConferences)
		}
	})
}

// ─── Test: end-to-end eMOS computation ───────────────────────────────────────

// TestIntegration_EndToEnd_eMOS flushes stats, triggers the eMOS job, and
// verifies a session_quality row is written with the expected outcome.
func TestIntegration_EndToEnd_eMOS(t *testing.T) {
	pool := testutil.StartPostgres(t)
	rdb := testutil.StartRedis(t)
	_, appID := testutil.SeedApp(t, pool)
	appIDStr := strconv.FormatInt(appID, 10)

	// 0% loss, 20ms RTT → eMOS ≈ 4.4 → outcome = "success"
	flusher := ingest.New(pool, rdb, noopLog(), t.TempDir())
	payload := samplePayload(appIDStr, "user-1", "conf-emos", "sess-emos", "conn-emos", 20, 0.0)
	flusher.Flush(context.Background(), []model.IngestPayload{payload})

	// Fetch the conference DB id.
	var confID int64
	if err := pool.QueryRow(context.Background(),
		`UPDATE conferences SET ended_at = now() WHERE app_id = $1 RETURNING id`, appID,
	).Scan(&confID); err != nil {
		t.Fatalf("end conference: %v", err)
	}

	// Run the eMOS job synchronously.
	if err := emos.RunJob(context.Background(), pool, confID, emos.DefaultLossCoeff, noopLog()); err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	// Verify session_quality row.
	var mosVal float32
	var outcome string
	err := pool.QueryRow(context.Background(), `
		SELECT sq.emos, sq.outcome
		FROM session_quality sq
		JOIN sessions s ON s.id = sq.session_id
		JOIN participants p ON p.id = s.participant_id
		WHERE p.conference_id = $1
		LIMIT 1
	`, confID).Scan(&mosVal, &outcome)
	if err != nil {
		t.Fatalf("query session_quality: %v", err)
	}
	if outcome != "success" {
		t.Errorf("outcome: want success, got %q", outcome)
	}
	if mosVal < 4.0 || mosVal > 5.0 {
		t.Errorf("emos: want 4.0–5.0 (≈4.4), got %v", mosVal)
	}
}

// ─── Test: rate limiting ──────────────────────────────────────────────────────

// TestIntegration_RateLimit sends requests through a real ingest-api httptest
// server with a rate limit of 5 and verifies the 6th request returns 429.
func TestIntegration_RateLimit(t *testing.T) {
	rdb := testutil.StartRedis(t)

	rl := ratelimit.New(rdb, ratelimit.Config{Max: 5, WindowSecs: 60}, noopLog())

	// no-op enqueue — we only care about auth + rate-limit behaviour.
	enqueue := func(p model.IngestPayload) error { return nil }

	srv := ingestserver.NewServer(
		context.Background(),
		nil, // db not needed for /v1/events with no-op enqueue
		rdb,
		noopLog(),
		testJWTSecret,
		rl,
		nil, // serverMasterKey
		nil, // serverRL
		enqueue,
		nil, // emosTrigger
	)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	client := ts.Client()
	makeReq := func() *http.Response {
		tok := makeJWT(t, "999", "conf-rl", "user-rl")
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/events", http.NoBody)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("rate limit request: %v", err)
		}
		return resp
	}

	// First 5 requests must succeed (not 429).
	for i := 1; i <= 5; i++ {
		resp := makeReq()
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("request %d: got 429 before limit reached", i)
		}
	}

	// 6th request must be 429.
	resp := makeReq()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("6th request: want 429, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("6th request: missing Retry-After header")
	}
}

// ─── Test: graceful shutdown drains worker pool ───────────────────────────────

// TestIntegration_GracefulShutdown enqueues items into the worker pool and
// verifies that Shutdown flushes every item to the database before returning.
func TestIntegration_GracefulShutdown(t *testing.T) {
	pool := testutil.StartPostgres(t)
	rdb := testutil.StartRedis(t)
	_, appID := testutil.SeedApp(t, pool)
	appIDStr := strconv.FormatInt(appID, 10)

	const itemCount = 50

	flusher := ingest.New(pool, rdb, noopLog(), t.TempDir())
	wp := worker.New(worker.Config{
		WorkerCount:     2,
		BatchSize:       itemCount + 1, // larger than queue so ticker drives flush
		FlushIntervalMs: 30_000,        // very long — shutdown must drain
		ChannelCap:      itemCount * 2,
	}, flusher.Flush, noopLog())

	for i := range itemCount {
		p := samplePayload(
			appIDStr, "user-1",
			fmt.Sprintf("conf-sd-%d", i),
			fmt.Sprintf("sess-sd-%d", i),
			fmt.Sprintf("conn-sd-%d", i),
			20, 0.0,
		)
		if err := wp.Enqueue(p); err != nil {
			t.Fatalf("enqueue item %d: %v", i, err)
		}
	}

	// Shutdown must block until all items are flushed.
	wp.Shutdown()

	// Each payload has 2 stat snapshots → expect 2*itemCount rows.
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM connection_stats`,
	).Scan(&count); err != nil {
		t.Fatalf("count connection_stats: %v", err)
	}
	want := itemCount * len(samplePayload(appIDStr, "u", "c", "s", "n", 20, 0).Events)
	if count != want {
		t.Errorf("connection_stats: want %d rows, got %d", want, count)
	}
}
