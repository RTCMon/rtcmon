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

// breakdownRequest builds a GET request for /v1/apps/{appId}/analytics/breakdown.
func breakdownRequest(appID int64, by, from, to string, sess *session.Data) *http.Request {
	url := fmt.Sprintf("/v1/apps/%d/analytics/breakdown", appID)
	sep := "?"
	for k, v := range map[string]string{"by": by, "from": from, "to": to} {
		if v != "" {
			url += sep + k + "=" + v
			sep = "&"
		}
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

// breakdownRequestStr allows injecting an arbitrary appId string.
func breakdownRequestStr(appIDStr, by, from, to string, sess *session.Data) *http.Request {
	url := fmt.Sprintf("/v1/apps/%s/analytics/breakdown?by=%s&from=%s&to=%s", appIDStr, by, from, to)
	r := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("appId", appIDStr)
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

func decodeBreakdown(t *testing.T, w *httptest.ResponseRecorder) handler.BreakdownResponse {
	t.Helper()
	var resp handler.BreakdownResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode breakdown: %v (body=%q)", err, w.Body.String())
	}
	return resp
}

// setupBreakdownApp creates org+app and registers cleanup.
func setupBreakdownApp(t *testing.T) (orgID, appID int64) {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('bd-org') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'bd-app', 'h') RETURNING id`,
		orgID,
	).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id=$1`, orgID)
	})
	return
}

// insertSession inserts conference→participant→session with dimension fields
// and an optional session_quality row. Returns sessID.
func insertBdSession(
	t *testing.T, appID int64,
	startedAt time.Time,
	browser, os, country, networkType string,
	emos float32, outcome string,
) int64 {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()

	extID := fmt.Sprintf("bd-%d-%d", appID, startedAt.UnixNano())
	var confID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO conferences (app_id, external_id, started_at) VALUES ($1, $2, $3) RETURNING id`,
		appID, extID, startedAt,
	).Scan(&confID); err != nil {
		t.Fatalf("insert conf: %v", err)
	}

	var partID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO participants (conference_id, user_id) VALUES ($1, 'u') RETURNING id`,
		confID,
	).Scan(&partID); err != nil {
		t.Fatalf("insert participant: %v", err)
	}

	var sessID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO sessions (participant_id, browser, os, country, network_type)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		partID, browser, os, country, networkType,
	).Scan(&sessID); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if outcome != "" {
		if _, err := pool.Exec(ctx,
			`INSERT INTO session_quality (session_id, emos, outcome) VALUES ($1, $2, $3)`,
			sessID, emos, outcome,
		); err != nil {
			t.Fatalf("insert session_quality: %v", err)
		}
	}
	return sessID
}

const (
	bdFrom = "2024-01-01T00:00:00Z"
	bdTo   = "2024-02-01T00:00:00Z"
)

var bdBase = time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

// ─── unit tests (no DB) ──────────────────────────────────────────────────────

func TestBreakdown_Unauthorized(t *testing.T) {
	h := handler.HandleGetAnalyticsBreakdown(nil, nil, noopLog())
	r := breakdownRequest(1, "browser", bdFrom, bdTo, nil)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusUnauthorized)
}

func TestBreakdown_InvalidAppId(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "admin"}
	h := handler.HandleGetAnalyticsBreakdown(nil, nil, noopLog())
	r := breakdownRequestStr("bad", "browser", bdFrom, bdTo, sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)
}

func TestBreakdown_MissingBy(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "admin"}
	h := handler.HandleGetAnalyticsBreakdown(nil, nil, noopLog())
	r := breakdownRequest(1, "", bdFrom, bdTo, sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)
	b := bodyJSON(t, w)
	if b["error"] != "by is required" {
		t.Errorf("error: got %v", b["error"])
	}
}

func TestBreakdown_InvalidDimension(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "admin"}
	h := handler.HandleGetAnalyticsBreakdown(nil, nil, noopLog())
	r := breakdownRequest(1, "foobar", bdFrom, bdTo, sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)
	b := bodyJSON(t, w)
	if b["error"] != "invalid dimension" {
		t.Errorf("error: got %v", b["error"])
	}
}

func TestBreakdown_MissingFrom(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "admin"}
	h := handler.HandleGetAnalyticsBreakdown(nil, nil, noopLog())
	r := breakdownRequest(1, "browser", "", bdTo, sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)
}

func TestBreakdown_MissingTo(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "admin"}
	h := handler.HandleGetAnalyticsBreakdown(nil, nil, noopLog())
	r := breakdownRequest(1, "browser", bdFrom, "", sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)
}

// ─── integration tests (require TEST_DB_URL) ─────────────────────────────────

func TestBreakdown_WrongApp(t *testing.T) {
	orgID1, _ := setupBreakdownApp(t)
	_, appID2 := setupBreakdownApp(t)

	sess := &session.Data{UserID: 1, OrgID: orgID1, Role: "admin"}
	h := handler.HandleGetAnalyticsBreakdown(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, breakdownRequest(appID2, "browser", bdFrom, bdTo, sess))
	requireStatus(t, w, http.StatusForbidden)
}

func TestBreakdown_EmptyData(t *testing.T) {
	orgID, appID := setupBreakdownApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	h := handler.HandleGetAnalyticsBreakdown(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, breakdownRequest(appID, "browser", bdFrom, bdTo, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeBreakdown(t, w)
	if resp.Dimension != "browser" {
		t.Errorf("dimension: want browser, got %q", resp.Dimension)
	}
	if resp.Data == nil || len(resp.Data) != 0 {
		t.Errorf("data must be [] not null or non-empty, got %v", resp.Data)
	}
}

func TestBreakdown_Browser(t *testing.T) {
	orgID, appID := setupBreakdownApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	insertBdSession(t, appID, bdBase, "Chrome", "Linux", "DE", "wifi", 4.0, "success")
	insertBdSession(t, appID, bdBase.Add(time.Hour), "Firefox", "Mac", "US", "lte", 3.5, "success")
	insertBdSession(t, appID, bdBase.Add(2*time.Hour), "Safari", "iOS", "UK", "wifi", 2.5, "degraded")

	h := handler.HandleGetAnalyticsBreakdown(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, breakdownRequest(appID, "browser", bdFrom, bdTo, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeBreakdown(t, w)
	if resp.Dimension != "browser" {
		t.Errorf("dimension: want browser, got %q", resp.Dimension)
	}
	if len(resp.Data) != 3 {
		t.Fatalf("want 3 browser rows, got %d", len(resp.Data))
	}
	// Each browser should appear exactly once.
	seen := make(map[string]bool)
	for _, item := range resp.Data {
		seen[item.Value] = true
		if item.Count != 1 {
			t.Errorf("browser %q: want count=1, got %d", item.Value, item.Count)
		}
	}
	for _, b := range []string{"Chrome", "Firefox", "Safari"} {
		if !seen[b] {
			t.Errorf("browser %q not in response", b)
		}
	}
}

func TestBreakdown_OS(t *testing.T) {
	orgID, appID := setupBreakdownApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	insertBdSession(t, appID, bdBase, "Chrome", "Linux", "DE", "wifi", 4.0, "success")
	insertBdSession(t, appID, bdBase.Add(time.Hour), "Chrome", "Windows", "US", "lte", 3.0, "degraded")

	h := handler.HandleGetAnalyticsBreakdown(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, breakdownRequest(appID, "os", bdFrom, bdTo, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeBreakdown(t, w)
	if resp.Dimension != "os" {
		t.Errorf("dimension: want os, got %q", resp.Dimension)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("want 2 OS rows, got %d", len(resp.Data))
	}
	seen := make(map[string]bool)
	for _, item := range resp.Data {
		seen[item.Value] = true
	}
	if !seen["Linux"] || !seen["Windows"] {
		t.Errorf("OS values missing: got %v", resp.Data)
	}
}

func TestBreakdown_Region(t *testing.T) {
	orgID, appID := setupBreakdownApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	insertBdSession(t, appID, bdBase, "Chrome", "Linux", "DE", "wifi", 4.0, "success")
	insertBdSession(t, appID, bdBase.Add(time.Hour), "Chrome", "Linux", "US", "wifi", 4.0, "success")

	h := handler.HandleGetAnalyticsBreakdown(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, breakdownRequest(appID, "region", bdFrom, bdTo, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeBreakdown(t, w)
	if resp.Dimension != "region" {
		t.Errorf("dimension: want region, got %q", resp.Dimension)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("want 2 region rows, got %d", len(resp.Data))
	}
}

func TestBreakdown_ConnectionType(t *testing.T) {
	orgID, appID := setupBreakdownApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	insertBdSession(t, appID, bdBase, "Chrome", "Linux", "DE", "wifi", 4.0, "success")
	insertBdSession(t, appID, bdBase.Add(time.Hour), "Chrome", "Linux", "DE", "lte", 3.0, "degraded")

	h := handler.HandleGetAnalyticsBreakdown(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, breakdownRequest(appID, "connection_type", bdFrom, bdTo, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeBreakdown(t, w)
	if resp.Dimension != "connection_type" {
		t.Errorf("dimension: want connection_type, got %q", resp.Dimension)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("want 2 connection_type rows, got %d", len(resp.Data))
	}
	seen := make(map[string]bool)
	for _, item := range resp.Data {
		seen[item.Value] = true
	}
	if !seen["wifi"] || !seen["lte"] {
		t.Errorf("connection_type values missing: got %v", resp.Data)
	}
}

func TestBreakdown_Ordered(t *testing.T) {
	orgID, appID := setupBreakdownApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	// Insert 5 Chrome, 3 Firefox, 1 Safari — expect descending order.
	for i := 0; i < 5; i++ {
		insertBdSession(t, appID, bdBase.Add(time.Duration(i)*time.Hour), "Chrome", "Linux", "DE", "wifi", 4.0, "success")
	}
	for i := 5; i < 8; i++ {
		insertBdSession(t, appID, bdBase.Add(time.Duration(i)*time.Hour), "Firefox", "Linux", "DE", "wifi", 4.0, "success")
	}
	insertBdSession(t, appID, bdBase.Add(9*time.Hour), "Safari", "iOS", "DE", "wifi", 4.0, "success")

	h := handler.HandleGetAnalyticsBreakdown(testPool(t), nil, noopLog())
	w := httptest.NewRecorder()
	h(w, breakdownRequest(appID, "browser", bdFrom, bdTo, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeBreakdown(t, w)
	if len(resp.Data) != 3 {
		t.Fatalf("want 3 rows, got %d", len(resp.Data))
	}
	// Verify descending order by count.
	for i := 1; i < len(resp.Data); i++ {
		if resp.Data[i].Count > resp.Data[i-1].Count {
			t.Errorf("not ordered desc: item[%d].count=%d > item[%d].count=%d",
				i, resp.Data[i].Count, i-1, resp.Data[i-1].Count)
		}
	}
	if resp.Data[0].Value != "Chrome" || resp.Data[0].Count != 5 {
		t.Errorf("first row: want Chrome/5, got %q/%d", resp.Data[0].Value, resp.Data[0].Count)
	}
}

func TestBreakdown_CacheHit(t *testing.T) {
	orgID, appID := setupBreakdownApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	mr, rdb := newMiniredisClient(t)
	insertBdSession(t, appID, bdBase, "Chrome", "Linux", "DE", "wifi", 4.0, "success")

	h := handler.HandleGetAnalyticsBreakdown(testPool(t), rdb, noopLog())

	// First request — populates cache.
	w1 := httptest.NewRecorder()
	h(w1, breakdownRequest(appID, "browser", bdFrom, bdTo, sess))
	requireStatus(t, w1, http.StatusOK)

	keys := mr.Keys()
	if len(keys) == 0 {
		t.Fatal("expected cache key after first request")
	}

	// Plant a sentinel to detect cache hit.
	sentinel := `{"dimension":"browser","data":[{"value":"sentinel","count":9999,"avg_emos":0,"success_rate":0}]}`
	for _, k := range keys {
		mr.Set(k, sentinel)
	}

	// Second request — must return sentinel from cache.
	w2 := httptest.NewRecorder()
	h(w2, breakdownRequest(appID, "browser", bdFrom, bdTo, sess))
	requireStatus(t, w2, http.StatusOK)

	resp := decodeBreakdown(t, w2)
	if len(resp.Data) == 0 || resp.Data[0].Value != "sentinel" {
		t.Errorf("expected sentinel from cache, got %+v", resp.Data)
	}
}
