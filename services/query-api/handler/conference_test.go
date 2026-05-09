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

// setupConferenceApp creates an org and an app owned by that org, returns their IDs.
func setupConferenceApp(t *testing.T) (orgID, appID int64) {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()

	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('conf-test-org') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'conf-test-app', 'hash') RETURNING id`,
		orgID,
	).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, orgID)
	})
	return
}

// insertConference inserts a conference row and returns its ID.
func insertConference(t *testing.T, appID int64, externalID string, startedAt time.Time, endedAt *time.Time) int64 {
	t.Helper()
	pool := testPool(t)
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO conferences (app_id, external_id, started_at, ended_at)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		appID, externalID, startedAt, endedAt,
	).Scan(&id); err != nil {
		t.Fatalf("insert conference %q: %v", externalID, err)
	}
	return id
}

// insertSessionQuality inserts participant → session → session_quality for a
// conference, simulating the eMOS job output.
func insertSessionQuality(t *testing.T, conferenceID int64, outcome string, emos float32) {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()

	var participantID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO participants (conference_id, user_id, display_name)
		 VALUES ($1, $2, '') RETURNING id`,
		conferenceID, fmt.Sprintf("user-%d", conferenceID),
	).Scan(&participantID); err != nil {
		t.Fatalf("insert participant: %v", err)
	}

	var sessionID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO sessions (participant_id) VALUES ($1) RETURNING id`,
		participantID,
	).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO session_quality (session_id, emos, outcome) VALUES ($1, $2, $3)`,
		sessionID, emos, outcome,
	); err != nil {
		t.Fatalf("insert session_quality: %v", err)
	}
}

// confRequest builds a GET request to /v1/apps/{appId}/conferences with chi URL
// params and optional session injected into context.
func confRequest(appID int64, queryParams string, sess *session.Data) *http.Request {
	url := fmt.Sprintf("/v1/apps/%d/conferences", appID)
	if queryParams != "" {
		url += "?" + queryParams
	}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("appId", itoa(appID))
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

// decodeConferenceList decodes a ConferenceListResponse from the recorder body.
func decodeConferenceList(t *testing.T, w *httptest.ResponseRecorder) handler.ConferenceListResponse {
	t.Helper()
	var resp handler.ConferenceListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v (body=%q)", err, w.Body.String())
	}
	return resp
}

// ─── unit tests (no DB) ──────────────────────────────────────────────────────

func TestConferenceList_Unauthorized(t *testing.T) {
	h := handler.HandleListConferences(nil, noopLog())
	r := confRequest(1, "", nil) // no session
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusUnauthorized)
}

// ─── integration tests (require TEST_DB_URL) ─────────────────────────────────

func TestConferenceList_Empty(t *testing.T) {
	orgID, appID := setupConferenceApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	h := handler.HandleListConferences(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, confRequest(appID, "", sess))

	requireStatus(t, w, http.StatusOK)
	resp := decodeConferenceList(t, w)
	if len(resp.Data) != 0 {
		t.Errorf("data: got %d items, want 0", len(resp.Data))
	}
	if resp.Pagination.Total != 0 {
		t.Errorf("total: got %d, want 0", resp.Pagination.Total)
	}
	if resp.Data == nil {
		t.Error("data must be [] not null")
	}
}

func TestConferenceList_Pagination(t *testing.T) {
	orgID, appID := setupConferenceApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 25; i++ {
		insertConference(t, appID, fmt.Sprintf("conf-%02d", i), base.Add(time.Duration(i)*time.Minute), nil)
	}

	h := handler.HandleListConferences(testPool(t), noopLog())

	// Page 1, limit 10 → 10 items, total 25.
	w := httptest.NewRecorder()
	h(w, confRequest(appID, "page=1&limit=10", sess))
	requireStatus(t, w, http.StatusOK)
	resp := decodeConferenceList(t, w)
	if len(resp.Data) != 10 {
		t.Errorf("page 1: got %d items, want 10", len(resp.Data))
	}
	if resp.Pagination.Total != 25 {
		t.Errorf("total: got %d, want 25", resp.Pagination.Total)
	}
	if resp.Pagination.Page != 1 || resp.Pagination.Limit != 10 {
		t.Errorf("pagination meta: %+v", resp.Pagination)
	}

	// Page 3, limit 10 → 5 items.
	w2 := httptest.NewRecorder()
	h(w2, confRequest(appID, "page=3&limit=10", sess))
	requireStatus(t, w2, http.StatusOK)
	resp2 := decodeConferenceList(t, w2)
	if len(resp2.Data) != 5 {
		t.Errorf("page 3: got %d items, want 5", len(resp2.Data))
	}
}

func TestConferenceList_FilterByDate(t *testing.T) {
	orgID, appID := setupConferenceApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	base := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	insertConference(t, appID, "early", base.Add(-24*time.Hour), nil)
	insertConference(t, appID, "target", base.Add(1*time.Hour), nil)
	insertConference(t, appID, "late", base.Add(48*time.Hour), nil)

	from := base.Format(time.RFC3339)
	to := base.Add(2 * time.Hour).Format(time.RFC3339)

	h := handler.HandleListConferences(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, confRequest(appID, "from="+from+"&to="+to, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeConferenceList(t, w)
	if len(resp.Data) != 1 {
		t.Errorf("got %d items, want 1", len(resp.Data))
	}
	if len(resp.Data) == 1 && resp.Data[0].ExternalID != "target" {
		t.Errorf("expected conference 'target', got %q", resp.Data[0].ExternalID)
	}
}

func TestConferenceList_FilterByQuality(t *testing.T) {
	orgID, appID := setupConferenceApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	base := time.Now().UTC().Truncate(time.Second)

	// 2 good (success), 1 degraded, 2 without quality data.
	for i, outcome := range []string{"success", "success", "degraded", "", ""} {
		cid := insertConference(t, appID, fmt.Sprintf("q-conf-%d", i), base.Add(time.Duration(i)*time.Minute), nil)
		if outcome != "" {
			insertSessionQuality(t, cid, outcome, 4.0)
		}
	}

	h := handler.HandleListConferences(testPool(t), noopLog())

	// quality=good → 2 results.
	wGood := httptest.NewRecorder()
	h(wGood, confRequest(appID, "quality=good", sess))
	requireStatus(t, wGood, http.StatusOK)
	respGood := decodeConferenceList(t, wGood)
	if respGood.Pagination.Total != 2 {
		t.Errorf("quality=good total: got %d, want 2", respGood.Pagination.Total)
	}
	for _, item := range respGood.Data {
		if item.Outcome == nil || *item.Outcome != "success" {
			t.Errorf("unexpected outcome %v in quality=good result", item.Outcome)
		}
	}

	// quality=degraded → 1 result.
	wDeg := httptest.NewRecorder()
	h(wDeg, confRequest(appID, "quality=degraded", sess))
	requireStatus(t, wDeg, http.StatusOK)
	respDeg := decodeConferenceList(t, wDeg)
	if respDeg.Pagination.Total != 1 {
		t.Errorf("quality=degraded total: got %d, want 1", respDeg.Pagination.Total)
	}
}

func TestConferenceList_LimitClamped(t *testing.T) {
	orgID, appID := setupConferenceApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 110; i++ {
		insertConference(t, appID, fmt.Sprintf("clamp-%03d", i), base.Add(time.Duration(i)*time.Minute), nil)
	}

	h := handler.HandleListConferences(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, confRequest(appID, "limit=200", sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeConferenceList(t, w)
	if len(resp.Data) > 100 {
		t.Errorf("got %d items with limit=200; must be ≤ 100", len(resp.Data))
	}
	if resp.Pagination.Limit != 100 {
		t.Errorf("pagination.limit: got %d, want 100", resp.Pagination.Limit)
	}
}

func TestConferenceList_WrongApp(t *testing.T) {
	// Create two separate orgs+apps.
	orgID1, _ := setupConferenceApp(t)
	_, appID2 := setupConferenceApp(t) // appID2 belongs to orgID2, not orgID1

	// Session belongs to org1 but requests app from org2.
	sess := &session.Data{UserID: 1, OrgID: orgID1, Role: "admin"}

	h := handler.HandleListConferences(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, confRequest(appID2, "", sess))
	requireStatus(t, w, http.StatusForbidden)
}
