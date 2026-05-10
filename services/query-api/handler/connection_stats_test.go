package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"

	"github.com/RTCMon/rtcmon/internal/session"
	"github.com/RTCMon/rtcmon/services/query-api/handler"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// statsRequest builds a GET request to /v1/connections/{connectionId}/stats
// with chi URL params and optional session injected into context.
func statsRequest(connectionID int64, queryParams string, sess *session.Data) *http.Request {
	url := fmt.Sprintf("/v1/connections/%d/stats", connectionID)
	if queryParams != "" {
		url += "?" + queryParams
	}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("connectionId", itoa(connectionID))
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

// statsRequestRaw builds a request without adding chi URL params — used to test
// invalid connectionId values.
func statsRequestRaw(connectionID string, queryParams string, sess *session.Data) *http.Request {
	url := fmt.Sprintf("/v1/connections/%s/stats", connectionID)
	if queryParams != "" {
		url += "?" + queryParams
	}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("connectionId", connectionID)
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

// setupStatsFixture creates org → app → conference → participant → session →
// connection and returns their IDs plus the org's session data.
func setupStatsFixture(t *testing.T) (orgID, connID int64, sess *session.Data, startedAt time.Time) {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()

	startedAt = time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)

	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('stats-org') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}

	var appID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'stats-app', 'hash') RETURNING id`,
		orgID,
	).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}

	var confID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO conferences (app_id, external_id, started_at) VALUES ($1, 'stats-conf', $2) RETURNING id`,
		appID, startedAt,
	).Scan(&confID); err != nil {
		t.Fatalf("insert conference: %v", err)
	}

	var partID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO participants (conference_id, user_id, display_name) VALUES ($1, 'u1', 'U1') RETURNING id`,
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
		`INSERT INTO connections (session_id, peer_id, started_at, ice_state, codec_audio, codec_video)
		 VALUES ($1, 'peer', $2, 'connected', 'opus', 'vp8') RETURNING id`,
		sessID, startedAt,
	).Scan(&connID); err != nil {
		t.Fatalf("insert connection: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, orgID)
	})

	sess = &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}
	return
}

// insertStatRows inserts n connection_stats rows for connID, one per second
// starting at base. Returns the timestamps.
func insertStatRows(t *testing.T, connID int64, base time.Time, n int, source string) []time.Time {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()

	if source == "" {
		source = "browser"
	}

	tss := make([]time.Time, n)
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		tss[i] = ts
		if _, err := pool.Exec(ctx, `
			INSERT INTO connection_stats
			  (connection_id, ts, packets_lost_rate, jitter_ms, rtt_ms,
			   bitrate_in_kbps, bitrate_out_kbps,
			   ewma_packets_lost_rate, ewma_jitter_ms, ewma_rtt_ms,
			   ewma_bitrate_in_kbps, ewma_bitrate_out_kbps,
			   fps, frame_width, frame_height, audio_level, concealment_ratio, source)
			VALUES ($1, $2, 0.01, 5.0, 20.0, 1000, 500, 0.01, 5.0, 20.0, 1000, 500,
			        30, 1280, 720, 0.8, 0.02, $3)`,
			connID, ts, source,
		); err != nil {
			t.Fatalf("insert stat row %d: %v", i, err)
		}
	}
	return tss
}

// decodeStatsResponse decodes a ConnectionStatsResponse from the recorder body.
func decodeStatsResponse(t *testing.T, w *httptest.ResponseRecorder) handler.ConnectionStatsResponse {
	t.Helper()
	var resp handler.ConnectionStatsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode stats response: %v (body=%q)", err, w.Body.String())
	}
	return resp
}

// newMiniredis creates a miniredis server and returns a Redis client pointed at it.
func newMiniredisClient(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// ─── unit tests (no DB) ──────────────────────────────────────────────────────

func TestStats_Unauthorized(t *testing.T) {
	h := handler.HandleGetConnectionStats(nil, nil, noopLog())
	r := statsRequest(1, "", nil) // no session
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusUnauthorized)
}

func TestStats_InvalidConnectionID(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "admin"}
	h := handler.HandleGetConnectionStats(nil, nil, noopLog())
	r := statsRequestRaw("not-a-number", "", sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)
}

func TestStats_InvalidFromParam(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "admin"}
	h := handler.HandleGetConnectionStats(nil, nil, noopLog())
	r := statsRequest(1, "from=not-a-date", sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)
}

func TestStats_InvalidToParam(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "admin"}
	h := handler.HandleGetConnectionStats(nil, nil, noopLog())
	r := statsRequest(1, "to=not-a-date", sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)
}

func TestStats_InvalidSourceParam(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "admin"}
	h := handler.HandleGetConnectionStats(nil, nil, noopLog())
	r := statsRequest(1, "source=invalid", sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)
}

// ─── integration tests (require TEST_DB_URL) ─────────────────────────────────

func TestStats_NotFound(t *testing.T) {
	orgID, _, _, _ := setupStatsFixture(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	h := handler.HandleGetConnectionStats(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, statsRequest(999999999, "", sess))
	requireStatus(t, w, http.StatusNotFound)

	b := bodyJSON(t, w)
	if b["error"] != "not found" {
		t.Errorf("error: got %v", b["error"])
	}
}

func TestStats_WrongApp(t *testing.T) {
	// Two separate orgs; session belongs to org1, connection is in org2's app.
	orgID1, _, _, _ := setupStatsFixture(t)
	_, connID2, _, _ := setupStatsFixture(t)

	sess := &session.Data{UserID: 1, OrgID: orgID1, Role: "admin"}

	h := handler.HandleGetConnectionStats(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, statsRequest(connID2, "", sess))
	requireStatus(t, w, http.StatusForbidden)
}

func TestStats_EmptyRange(t *testing.T) {
	_, connID, sess, startedAt := setupStatsFixture(t)

	// Query a range far in the future with no rows.
	future := startedAt.Add(24 * time.Hour)
	from := future.Format(time.RFC3339)
	to := future.Add(time.Hour).Format(time.RFC3339)

	h := handler.HandleGetConnectionStats(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, statsRequest(connID, "from="+from+"&to="+to, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeStatsResponse(t, w)
	if resp.Data == nil {
		t.Error("data must be [] not null")
	}
	if len(resp.Data) != 0 {
		t.Errorf("expected 0 rows, got %d", len(resp.Data))
	}
}

func TestStats_ValidRange(t *testing.T) {
	_, connID, sess, startedAt := setupStatsFixture(t)

	// Insert 60 rows starting at startedAt.
	insertStatRows(t, connID, startedAt, 60, "browser")

	from := startedAt.Format(time.RFC3339)
	to := startedAt.Add(59 * time.Second).Format(time.RFC3339)

	h := handler.HandleGetConnectionStats(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, statsRequest(connID, "from="+from+"&to="+to, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeStatsResponse(t, w)
	if len(resp.Data) != 60 {
		t.Errorf("expected 60 rows, got %d", len(resp.Data))
	}

	// Verify ascending order.
	for i := 1; i < len(resp.Data); i++ {
		if !resp.Data[i].TS.After(resp.Data[i-1].TS) && resp.Data[i].TS != resp.Data[i-1].TS {
			t.Errorf("rows not ordered by ts: row[%d]=%v row[%d]=%v", i-1, resp.Data[i-1].TS, i, resp.Data[i].TS)
		}
	}

	// Verify source field is present on every row.
	for i, row := range resp.Data {
		if row.Source == "" {
			t.Errorf("row[%d] has empty source", i)
		}
	}
}

func TestStats_DefaultRange(t *testing.T) {
	_, connID, sess, startedAt := setupStatsFixture(t)

	// Insert 5 rows.
	insertStatRows(t, connID, startedAt, 5, "browser")

	// No from/to — should default to connection's started_at / (started_at + 1 hour)
	// because the connection has no ended_at.
	h := handler.HandleGetConnectionStats(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, statsRequest(connID, "", sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeStatsResponse(t, w)
	if len(resp.Data) != 5 {
		t.Errorf("expected 5 rows, got %d", len(resp.Data))
	}
}

func TestStats_FilterBySource_Browser(t *testing.T) {
	_, connID, sess, startedAt := setupStatsFixture(t)

	// Insert 3 browser rows and 2 server rows interleaved.
	pool := testPool(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		src := "browser"
		if i%2 == 1 {
			src = "server"
		}
		ts := startedAt.Add(time.Duration(i) * time.Second)
		if _, err := pool.Exec(ctx, `
			INSERT INTO connection_stats
			  (connection_id, ts, source)
			VALUES ($1, $2, $3)`,
			connID, ts, src,
		); err != nil {
			t.Fatalf("insert stat: %v", err)
		}
	}

	from := startedAt.Format(time.RFC3339)
	to := startedAt.Add(10 * time.Second).Format(time.RFC3339)

	h := handler.HandleGetConnectionStats(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, statsRequest(connID, "from="+from+"&to="+to+"&source=browser", sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeStatsResponse(t, w)
	if len(resp.Data) != 3 {
		t.Errorf("expected 3 browser rows, got %d", len(resp.Data))
	}
	for _, row := range resp.Data {
		if row.Source != "browser" {
			t.Errorf("got source=%q, want browser", row.Source)
		}
	}
}

func TestStats_FilterBySource_Server(t *testing.T) {
	_, connID, sess, startedAt := setupStatsFixture(t)

	pool := testPool(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		src := "server"
		if i < 2 {
			src = "browser"
		}
		ts := startedAt.Add(time.Duration(i) * time.Second)
		if _, err := pool.Exec(ctx, `
			INSERT INTO connection_stats (connection_id, ts, source) VALUES ($1, $2, $3)`,
			connID, ts, src,
		); err != nil {
			t.Fatalf("insert stat: %v", err)
		}
	}

	from := startedAt.Format(time.RFC3339)
	to := startedAt.Add(10 * time.Second).Format(time.RFC3339)

	h := handler.HandleGetConnectionStats(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, statsRequest(connID, "from="+from+"&to="+to+"&source=server", sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeStatsResponse(t, w)
	if len(resp.Data) != 2 {
		t.Errorf("expected 2 server rows, got %d", len(resp.Data))
	}
	for _, row := range resp.Data {
		if row.Source != "server" {
			t.Errorf("got source=%q, want server", row.Source)
		}
	}
}

func TestStats_NoSourceFilter(t *testing.T) {
	_, connID, sess, startedAt := setupStatsFixture(t)

	// Insert 2 browser + 2 server rows.
	pool := testPool(t)
	ctx := context.Background()
	for i, src := range []string{"browser", "server", "browser", "server"} {
		ts := startedAt.Add(time.Duration(i) * time.Second)
		if _, err := pool.Exec(ctx, `
			INSERT INTO connection_stats (connection_id, ts, source) VALUES ($1, $2, $3)`,
			connID, ts, src,
		); err != nil {
			t.Fatalf("insert stat: %v", err)
		}
	}

	from := startedAt.Format(time.RFC3339)
	to := startedAt.Add(10 * time.Second).Format(time.RFC3339)

	h := handler.HandleGetConnectionStats(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, statsRequest(connID, "from="+from+"&to="+to, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeStatsResponse(t, w)
	if len(resp.Data) != 4 {
		t.Errorf("expected 4 rows (browser+server), got %d", len(resp.Data))
	}

	// Verify ordered by ts ascending.
	for i := 1; i < len(resp.Data); i++ {
		if resp.Data[i].TS.Before(resp.Data[i-1].TS) {
			t.Errorf("rows not ordered: row[%d]=%v before row[%d]=%v",
				i, resp.Data[i].TS, i-1, resp.Data[i-1].TS)
		}
	}
}

func TestStats_CacheMiss_PopulatesCache(t *testing.T) {
	_, connID, sess, startedAt := setupStatsFixture(t)
	insertStatRows(t, connID, startedAt, 2, "browser")

	_, rdb := newMiniredisClient(t)

	from := startedAt.Format(time.RFC3339)
	to := startedAt.Add(time.Hour).Format(time.RFC3339)

	h := handler.HandleGetConnectionStats(testPool(t), rdb, noopLog())
	w := httptest.NewRecorder()
	h(w, statsRequest(connID, "from="+from+"&to="+to, sess))
	requireStatus(t, w, http.StatusOK)

	// Verify the cache key was populated.
	cacheKey := fmt.Sprintf("stats:%d:%s:%s:all",
		connID,
		startedAt.UTC().Format(time.RFC3339),
		startedAt.Add(time.Hour).UTC().Format(time.RFC3339),
	)
	val, err := rdb.Get(context.Background(), cacheKey).Bytes()
	if err != nil {
		t.Fatalf("cache key not found: %v", err)
	}

	var cached handler.ConnectionStatsResponse
	if err := json.Unmarshal(val, &cached); err != nil {
		t.Fatalf("cached value is not valid JSON: %v", err)
	}
	if len(cached.Data) != 2 {
		t.Errorf("cached data: got %d rows, want 2", len(cached.Data))
	}
}

func TestStats_CacheHit(t *testing.T) {
	_, connID, sess, startedAt := setupStatsFixture(t)

	mr, rdb := newMiniredisClient(t)

	from := startedAt.Format(time.RFC3339)
	to := startedAt.Add(time.Hour).Format(time.RFC3339)

	h := handler.HandleGetConnectionStats(testPool(t), rdb, noopLog())

	// First request — populates cache (no rows in DB, so data=[]).
	w1 := httptest.NewRecorder()
	h(w1, statsRequest(connID, "from="+from+"&to="+to, sess))
	requireStatus(t, w1, http.StatusOK)

	// Pre-seed the cache with known data to confirm the second request reads it.
	fakeData := `{"data":[{"ts":"2024-01-01T00:00:00Z","rtt_ms":null,"jitter_ms":null,"packet_loss_rate":null,"bitrate_in_kbps":null,"bitrate_out_kbps":null,"ewma_rtt_ms":null,"ewma_jitter_ms":null,"ewma_packet_loss_rate":null,"ewma_bitrate_in_kbps":null,"ewma_bitrate_out_kbps":null,"fps":null,"frame_width":null,"frame_height":null,"audio_level":null,"concealment_ratio":null,"source":"cache-marker"}]}`
	cacheKey := fmt.Sprintf("stats:%d:%s:%s:all",
		connID,
		startedAt.UTC().Format(time.RFC3339),
		startedAt.Add(time.Hour).UTC().Format(time.RFC3339),
	)
	mr.Set(cacheKey, fakeData)

	// Second request — must be served from cache.
	w2 := httptest.NewRecorder()
	h(w2, statsRequest(connID, "from="+from+"&to="+to, sess))
	requireStatus(t, w2, http.StatusOK)

	var cached handler.ConnectionStatsResponse
	if err := json.NewDecoder(w2.Body).Decode(&cached); err != nil {
		t.Fatalf("decode cached: %v", err)
	}
	if len(cached.Data) != 1 || cached.Data[0].Source != "cache-marker" {
		t.Errorf("second response not served from cache: %+v", cached.Data)
	}
}
