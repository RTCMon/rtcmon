package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/RTCMon/rtcmon/internal/session"
	"github.com/RTCMon/rtcmon/services/query-api/handler"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// overviewRequest builds a GET request for /v1/apps/{appId}/analytics/overview.
func overviewRequest(appID int64, from, to string, sess *session.Data) *http.Request {
	url := fmt.Sprintf("/v1/apps/%d/analytics/overview", appID)
	if from != "" || to != "" {
		url += fmt.Sprintf("?from=%s&to=%s", from, to)
	}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("appId", fmt.Sprintf("%d", appID))
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

// overviewRequestStr builds a request where appId is an arbitrary string.
func overviewRequestStr(appIDStr, from, to string, sess *session.Data) *http.Request {
	url := fmt.Sprintf("/v1/apps/%s/analytics/overview?from=%s&to=%s", appIDStr, from, to)
	r := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("appId", appIDStr)
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

// decodeOverview decodes an OverviewResponse from the recorder body.
func decodeOverview(t *testing.T, w *httptest.ResponseRecorder) handler.OverviewResponse {
	t.Helper()
	var resp handler.OverviewResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode overview: %v (body=%q)", err, w.Body.String())
	}
	return resp
}

// setupOverviewApp creates org+app and registers cleanup.
func setupOverviewApp(t *testing.T) (orgID, appID int64) {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('ov-org') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'ov-app', 'h') RETURNING id`,
		orgID,
	).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id=$1`, orgID)
	})
	return
}

// insertConfWithQuality inserts a full conference hierarchy:
//
//	conference → participant → session → connection + session_quality
//
// Returns (confID, partID, sessID, connID).
func insertConfWithQuality(
	t *testing.T, appID int64,
	startedAt time.Time, endedAt *time.Time,
	joinedAt time.Time, connStartedAt time.Time,
	emos float32, outcome string,
) (confID, partID, sessID, connID int64) {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()

	extID := fmt.Sprintf("conf-%d-%d", appID, startedAt.UnixNano())
	if err := pool.QueryRow(ctx,
		`INSERT INTO conferences (app_id, external_id, started_at, ended_at) VALUES ($1,$2,$3,$4) RETURNING id`,
		appID, extID, startedAt, endedAt,
	).Scan(&confID); err != nil {
		t.Fatalf("insert conf: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO participants (conference_id, user_id, joined_at) VALUES ($1,'u',$2) RETURNING id`,
		confID, joinedAt,
	).Scan(&partID); err != nil {
		t.Fatalf("insert participant: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO sessions (participant_id) VALUES ($1) RETURNING id`,
		partID,
	).Scan(&sessID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO connections (session_id, started_at) VALUES ($1,$2) RETURNING id`,
		sessID, connStartedAt,
	).Scan(&connID); err != nil {
		t.Fatalf("insert connection: %v", err)
	}
	if outcome != "" {
		if _, err := pool.Exec(ctx,
			`INSERT INTO session_quality (session_id, emos, outcome) VALUES ($1,$2,$3)`,
			sessID, emos, outcome,
		); err != nil {
			t.Fatalf("insert session_quality: %v", err)
		}
	}
	return
}

const (
	ovFrom = "2024-01-01T00:00:00Z"
	ovTo   = "2024-02-01T00:00:00Z"
)

// ─── unit tests (no DB) ──────────────────────────────────────────────────────

func TestOverview_Unauthorized(t *testing.T) {
	h := handler.HandleGetAnalyticsOverview(nil, nil, noopLog())
	r := overviewRequest(1, ovFrom, ovTo, nil)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusUnauthorized)
}

func TestOverview_InvalidAppId(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "admin"}
	h := handler.HandleGetAnalyticsOverview(nil, nil, noopLog())
	r := overviewRequestStr("bad", ovFrom, ovTo, sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)
}

func TestOverview_MissingFrom(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "admin"}
	h := handler.HandleGetAnalyticsOverview(nil, nil, noopLog())

	url := "/v1/apps/1/analytics/overview?to=" + ovTo
	r := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("appId", "1")
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	ctx = session.WithContext(ctx, sess)
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)
	b := bodyJSON(t, w)
	if b["error"] != "from is required" {
		t.Errorf("error: got %v", b["error"])
	}
}

func TestOverview_MissingTo(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "admin"}
	h := handler.HandleGetAnalyticsOverview(nil, nil, noopLog())

	url := "/v1/apps/1/analytics/overview?from=" + ovFrom
	r := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("appId", "1")
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	ctx = session.WithContext(ctx, sess)
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)
	b := bodyJSON(t, w)
	if b["error"] != "to is required" {
		t.Errorf("error: got %v", b["error"])
	}
}

// ─── integration tests (require TEST_DB_URL) ─────────────────────────────────

func TestOverview_WrongApp(t *testing.T) {
	orgID1, _ := setupOverviewApp(t)
	_, appID2 := setupOverviewApp(t) // different org

	sess := &session.Data{UserID: 1, OrgID: orgID1, Role: "admin"}
	h := handler.HandleGetAnalyticsOverview(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, overviewRequest(appID2, ovFrom, ovTo, sess))
	requireStatus(t, w, http.StatusForbidden)
}

func TestOverview_EmptyRange(t *testing.T) {
	orgID, appID := setupOverviewApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	h := handler.HandleGetAnalyticsOverview(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, overviewRequest(appID, ovFrom, ovTo, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeOverview(t, w)
	if resp.TotalConferences != 0 || resp.TotalParticipants != 0 ||
		resp.AvgEMOS != 0 || resp.CallSuccessRate != 0 ||
		resp.P50SetupTimeMs != 0 || resp.P95SetupTimeMs != 0 {
		t.Errorf("expected all zeros for empty range, got %+v", resp)
	}
}

func TestOverview_KnownDataset(t *testing.T) {
	orgID, appID := setupOverviewApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	// Insert 10 conferences, each with a session+connection+quality.
	// All have: eMOS=4.0 (success), setup time = 200ms.
	base := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	endedAt := base.Add(time.Hour)
	joinedAt := base
	connStart := base.Add(200 * time.Millisecond)

	for i := 0; i < 10; i++ {
		st := base.Add(time.Duration(i) * time.Hour)
		et := st.Add(time.Hour)
		insertConfWithQuality(t, appID, st, &et, joinedAt, connStart, 4.0, "success")
	}

	h := handler.HandleGetAnalyticsOverview(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, overviewRequest(appID, ovFrom, ovTo, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeOverview(t, w)
	if resp.TotalConferences != 10 {
		t.Errorf("total_conferences: want 10, got %d", resp.TotalConferences)
	}
	if resp.TotalParticipants != 10 {
		t.Errorf("total_participants: want 10, got %d", resp.TotalParticipants)
	}
	if resp.CallSuccessRate != 1.0 {
		t.Errorf("call_success_rate: want 1.0, got %v", resp.CallSuccessRate)
	}
	if resp.AvgEMOS < 3.9 || resp.AvgEMOS > 4.1 {
		t.Errorf("avg_emos: want ≈4.0, got %v", resp.AvgEMOS)
	}
	if resp.P50SetupTimeMs != 200 {
		t.Errorf("p50_setup_time_ms: want 200, got %d", resp.P50SetupTimeMs)
	}
	_ = endedAt
}

func TestOverview_DateFilter(t *testing.T) {
	orgID, appID := setupOverviewApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	// Insert 5 conferences in Jan 2024, 15 outside the range.
	inRange := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	outRange := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	endedAt := inRange.Add(time.Hour)

	for i := 0; i < 5; i++ {
		insertConfWithQuality(t, appID, inRange.Add(time.Duration(i)*time.Hour), &endedAt,
			inRange, inRange.Add(100*time.Millisecond), 4.0, "success")
	}
	for i := 0; i < 15; i++ {
		et := outRange.Add(time.Hour)
		insertConfWithQuality(t, appID, outRange.Add(time.Duration(i)*time.Hour), &et,
			outRange, outRange.Add(100*time.Millisecond), 3.0, "degraded")
	}

	h := handler.HandleGetAnalyticsOverview(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, overviewRequest(appID, ovFrom, ovTo, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeOverview(t, w)
	if resp.TotalConferences != 5 {
		t.Errorf("date filter: want 5 conferences, got %d", resp.TotalConferences)
	}
}

func TestOverview_CacheHit(t *testing.T) {
	orgID, appID := setupOverviewApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	mr, rdb := newMiniredisClient(t)

	h := handler.HandleGetAnalyticsOverview(testPool(t), rdb, noopLog())

	// First request — cache miss, populates Redis.
	w1 := httptest.NewRecorder()
	h(w1, overviewRequest(appID, ovFrom, ovTo, sess))
	requireStatus(t, w1, http.StatusOK)

	// Verify key was stored.
	keys := mr.Keys()
	if len(keys) == 0 {
		t.Fatal("expected cache key to be set after first request")
	}

	// Corrupt the DB response by planting a sentinel in Redis.
	sentinel := `{"total_conferences":9999,"call_success_rate":0,"avg_emos":0,"p50_setup_time_ms":0,"p95_setup_time_ms":0,"total_participants":0}`
	for _, k := range keys {
		mr.Set(k, sentinel)
	}

	// Second request — must serve from cache (sentinel value).
	w2 := httptest.NewRecorder()
	h(w2, overviewRequest(appID, ovFrom, ovTo, sess))
	requireStatus(t, w2, http.StatusOK)

	resp := decodeOverview(t, w2)
	if resp.TotalConferences != 9999 {
		t.Errorf("expected cache sentinel value 9999, got %d", resp.TotalConferences)
	}
}

func TestOverview_SuccessRate(t *testing.T) {
	orgID, appID := setupOverviewApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	base := time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC)
	endedAt := base.Add(time.Hour)

	// 8 success + 2 failed = 10 conferences with eMOS.
	for i := 0; i < 8; i++ {
		st := base.Add(time.Duration(i) * time.Hour)
		insertConfWithQuality(t, appID, st, &endedAt, base, base.Add(100*time.Millisecond), 4.0, "success")
	}
	for i := 8; i < 10; i++ {
		st := base.Add(time.Duration(i) * time.Hour)
		insertConfWithQuality(t, appID, st, &endedAt, base, base.Add(100*time.Millisecond), 1.5, "failed")
	}

	h := handler.HandleGetAnalyticsOverview(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, overviewRequest(appID, ovFrom, ovTo, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeOverview(t, w)
	const wantRate = 0.80
	const tol = 0.01
	if resp.CallSuccessRate < wantRate-tol || resp.CallSuccessRate > wantRate+tol {
		t.Errorf("call_success_rate: want %.2f, got %v", wantRate, resp.CallSuccessRate)
	}
}
