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

// callsRequest builds a GET request to /v1/users/{userId}/calls with chi URL
// params, query string, and optional session.
func callsRequest(userID string, queryParams string, sess *session.Data) *http.Request {
	url := fmt.Sprintf("/v1/users/%s/calls", userID)
	if queryParams != "" {
		url += "?" + queryParams
	}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("userId", userID)
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

// setupCallsApp creates an org + app and returns their IDs.
func setupCallsApp(t *testing.T) (orgID, appID int64) {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()

	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('calls-org') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'calls-app', 'hash') RETURNING id`,
		orgID,
	).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, orgID)
	})
	return
}

// insertUserConference inserts conference + participant for a given userID and
// returns the conference ID. If endedAt is non-zero it sets ended_at.
func insertUserConference(t *testing.T, appID int64, userID string, startedAt time.Time, endedAt *time.Time) (confID, partID int64) {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()

	extID := fmt.Sprintf("conf-%s-%d", userID, startedAt.UnixNano())
	if err := pool.QueryRow(ctx,
		`INSERT INTO conferences (app_id, external_id, started_at, ended_at)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		appID, extID, startedAt, endedAt,
	).Scan(&confID); err != nil {
		t.Fatalf("insert conference: %v", err)
	}

	if err := pool.QueryRow(ctx,
		`INSERT INTO participants (conference_id, user_id, display_name)
		 VALUES ($1, $2, $2) RETURNING id`,
		confID, userID,
	).Scan(&partID); err != nil {
		t.Fatalf("insert participant: %v", err)
	}
	return
}

// insertSessionForParticipant inserts a session row for a participant.
func insertSessionForParticipant(t *testing.T, partID int64, browser, os, country string) int64 {
	t.Helper()
	pool := testPool(t)
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO sessions (participant_id, browser, os, country)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		partID, browser, os, country,
	).Scan(&id); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return id
}

// decodeCallsResponse decodes a UserCallsResponse from the recorder body.
func decodeCallsResponse(t *testing.T, w *httptest.ResponseRecorder) handler.UserCallsResponse {
	t.Helper()
	var resp handler.UserCallsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode calls response: %v (body=%q)", err, w.Body.String())
	}
	return resp
}

// ─── unit tests (no DB) ──────────────────────────────────────────────────────

func TestUserHistory_Unauthorized(t *testing.T) {
	h := handler.HandleGetUserCalls(nil, noopLog())
	r := callsRequest("user-1", "appId=1", nil) // no session
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusUnauthorized)
}

func TestUserHistory_MissingAppID(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "admin"}
	h := handler.HandleGetUserCalls(nil, noopLog())
	r := callsRequest("user-1", "", sess) // no appId param
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)

	b := bodyJSON(t, w)
	if b["error"] != "appId is required" {
		t.Errorf("error: got %v", b["error"])
	}
}

func TestUserHistory_InvalidAppID(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "admin"}
	h := handler.HandleGetUserCalls(nil, noopLog())
	r := callsRequest("user-1", "appId=not-an-int", sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)
}

// ─── integration tests (require TEST_DB_URL) ─────────────────────────────────

func TestUserHistory_WrongApp(t *testing.T) {
	orgID1, _ := setupCallsApp(t)
	_, appID2 := setupCallsApp(t) // belongs to a different org

	sess := &session.Data{UserID: 1, OrgID: orgID1, Role: "admin"}
	h := handler.HandleGetUserCalls(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, callsRequest("user-1", fmt.Sprintf("appId=%d", appID2), sess))
	requireStatus(t, w, http.StatusForbidden)
}

func TestUserHistory_NoCallsForUser(t *testing.T) {
	orgID, appID := setupCallsApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	h := handler.HandleGetUserCalls(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, callsRequest("unknown-user", fmt.Sprintf("appId=%d", appID), sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeCallsResponse(t, w)
	if resp.Data == nil {
		t.Error("data must be [] not null")
	}
	if len(resp.Data) != 0 {
		t.Errorf("expected 0 items, got %d", len(resp.Data))
	}
}

func TestUserHistory_ValidUser(t *testing.T) {
	orgID, appID := setupCallsApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	base := time.Now().UTC().Truncate(time.Second)
	// Insert 3 conferences for the same user at different times.
	for i := 0; i < 3; i++ {
		startedAt := base.Add(-time.Duration(i) * time.Hour)
		endedAt := startedAt.Add(30 * time.Minute)
		insertUserConference(t, appID, "user-hist", startedAt, &endedAt)
	}

	h := handler.HandleGetUserCalls(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, callsRequest("user-hist", fmt.Sprintf("appId=%d", appID), sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeCallsResponse(t, w)
	if len(resp.Data) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(resp.Data))
	}

	// Verify descending order.
	for i := 1; i < len(resp.Data); i++ {
		if resp.Data[i].StartedAt.After(resp.Data[i-1].StartedAt) {
			t.Errorf("calls not in descending order: item[%d]=%v after item[%d]=%v",
				i, resp.Data[i].StartedAt, i-1, resp.Data[i-1].StartedAt)
		}
	}
}

func TestUserHistory_LimitApplied(t *testing.T) {
	orgID, appID := setupCallsApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 10; i++ {
		startedAt := base.Add(-time.Duration(i) * time.Hour)
		endedAt := startedAt.Add(time.Hour)
		insertUserConference(t, appID, "user-limit", startedAt, &endedAt)
	}

	h := handler.HandleGetUserCalls(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, callsRequest("user-limit", fmt.Sprintf("appId=%d&limit=3", appID), sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeCallsResponse(t, w)
	if len(resp.Data) != 3 {
		t.Errorf("expected 3 items with limit=3, got %d", len(resp.Data))
	}
}

func TestUserHistory_LimitClamped(t *testing.T) {
	orgID, appID := setupCallsApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	// Insert 60 calls, request limit=200 → should get at most 50.
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 60; i++ {
		startedAt := base.Add(-time.Duration(i) * time.Minute)
		endedAt := startedAt.Add(10 * time.Minute)
		insertUserConference(t, appID, "user-clamp", startedAt, &endedAt)
	}

	h := handler.HandleGetUserCalls(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, callsRequest("user-clamp", fmt.Sprintf("appId=%d&limit=200", appID), sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeCallsResponse(t, w)
	if len(resp.Data) > 50 {
		t.Errorf("expected at most 50 items with limit=200, got %d", len(resp.Data))
	}
}

func TestUserHistory_DurationComputed(t *testing.T) {
	orgID, appID := setupCallsApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	base := time.Now().UTC().Truncate(time.Second)

	// Ended call: duration = 30 minutes = 1800 seconds.
	endedAt := base.Add(30 * time.Minute)
	insertUserConference(t, appID, "user-dur", base, &endedAt)

	// Ongoing call (no ended_at).
	insertUserConference(t, appID, "user-dur", base.Add(-time.Hour), nil)

	h := handler.HandleGetUserCalls(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, callsRequest("user-dur", fmt.Sprintf("appId=%d", appID), sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeCallsResponse(t, w)
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(resp.Data))
	}

	// Most recent (ended call) is first.
	ended := resp.Data[0]
	if ended.DurationSecs == nil {
		t.Error("ended call: duration_secs must not be null")
	} else if *ended.DurationSecs != 1800 {
		t.Errorf("ended call: duration_secs got %d, want 1800", *ended.DurationSecs)
	}
	if ended.EndedAt == nil {
		t.Error("ended call: ended_at must not be null")
	}

	// Older (ongoing call) is second.
	ongoing := resp.Data[1]
	if ongoing.DurationSecs != nil {
		t.Errorf("ongoing call: duration_secs must be null, got %d", *ongoing.DurationSecs)
	}
	if ongoing.EndedAt != nil {
		t.Errorf("ongoing call: ended_at must be null, got %v", *ongoing.EndedAt)
	}
}

func TestUserHistory_BrowserOSCountry(t *testing.T) {
	orgID, appID := setupCallsApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	base := time.Now().UTC().Truncate(time.Second)
	endedAt := base.Add(time.Hour)
	_, partID := insertUserConference(t, appID, "user-device", base, &endedAt)
	insertSessionForParticipant(t, partID, "Firefox", "Linux", "DE")

	h := handler.HandleGetUserCalls(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, callsRequest("user-device", fmt.Sprintf("appId=%d", appID), sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeCallsResponse(t, w)
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 call, got %d", len(resp.Data))
	}

	item := resp.Data[0]
	if item.Browser != "Firefox" {
		t.Errorf("browser: got %q, want Firefox", item.Browser)
	}
	if item.OS != "Linux" {
		t.Errorf("os: got %q, want Linux", item.OS)
	}
	if item.Country != "DE" {
		t.Errorf("country: got %q, want DE", item.Country)
	}
}
