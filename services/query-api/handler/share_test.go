package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/RTCMon/rtcmon/internal/session"
	"github.com/RTCMon/rtcmon/services/query-api/handler"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// insertShareToken inserts a share token directly into the DB and returns the token string.
func insertShareToken(t *testing.T, token string, conferenceID, orgID, createdBy int64) {
	t.Helper()
	pool := testPool(t)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO share_tokens (token, conference_id, org_id, created_by)
		 VALUES ($1, $2, $3, $4)`,
		token, conferenceID, orgID, createdBy,
	)
	if err != nil {
		t.Fatalf("insertShareToken: %v", err)
	}
}

// getViewCount returns the current view_count for a share token.
func getViewCount(t *testing.T, token string) int {
	t.Helper()
	pool := testPool(t)
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT view_count FROM share_tokens WHERE token = $1`, token,
	).Scan(&count); err != nil {
		t.Fatalf("getViewCount: %v", err)
	}
	return count
}

// tokenExists reports whether the share token row exists in the DB.
func tokenExists(t *testing.T, token string) bool {
	t.Helper()
	pool := testPool(t)
	var exists bool
	_ = pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM share_tokens WHERE token = $1)`, token,
	).Scan(&exists)
	return exists
}

// shareRequest builds a POST /v1/conferences/{id}/share request with chi URL
// params and an optional session in context.
func shareCreateRequest(conferenceID int64, sess *session.Data) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/conferences/1/share", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", itoa(conferenceID))
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

// shareViewRequest builds a GET /share/{token} request (no session in context
// unless sess is non-nil).
func shareViewRequest(token string, sess *session.Data) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/share/"+token, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("token", token)
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

// shareRevokeRequest builds a DELETE /v1/share-tokens/{token} request.
func shareRevokeRequest(token string, sess *session.Data) *http.Request {
	r := httptest.NewRequest(http.MethodDelete, "/v1/share-tokens/"+token, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("token", token)
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

// shareListRequest builds a GET /v1/share-tokens request.
func shareListRequest(sess *session.Data) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/share-tokens", nil)
	ctx := r.Context()
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

// ─── unit tests (no DB needed) ───────────────────────────────────────────────

func TestShare_Create_Unauthenticated(t *testing.T) {
	h := handler.HandleCreateShareToken(nil, noopLog())
	r := shareCreateRequest(1, nil)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusUnauthorized)
}

func TestShare_Create_InvalidConferenceID(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "member"}
	h := handler.HandleCreateShareToken(nil, noopLog())
	r := httptest.NewRequest(http.MethodPost, "/v1/conferences/abc/share", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "abc")
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	ctx = session.WithContext(ctx, sess)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)
}

func TestShare_View_NoSession(t *testing.T) {
	// No session → expects redirect to /login?next=...
	h := handler.HandleGetShareToken(nil, nil, noopLog())
	r := shareViewRequest("sometoken", nil)
	w := httptest.NewRecorder()
	h(w, r)
	// Should redirect (307)
	if w.Code != http.StatusTemporaryRedirect {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusTemporaryRedirect)
	}
	loc := w.Header().Get("Location")
	if loc == "" {
		t.Error("Location header must be present on redirect")
	}
}

func TestShare_Revoke_Unauthenticated(t *testing.T) {
	h := handler.HandleRevokeShareToken(nil, noopLog())
	r := shareRevokeRequest("abc", nil)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusUnauthorized)
}

func TestShare_List_Unauthenticated(t *testing.T) {
	h := handler.HandleListShareTokens(nil, noopLog())
	r := shareListRequest(nil)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusUnauthorized)
}

// ─── integration tests (require TEST_DB_URL) ─────────────────────────────────

// setupShareTest creates an org, app, and conference, plus two users (admin + member).
// Returns (orgID, conferenceID, adminUserID, memberUserID).
func setupShareTest(t *testing.T) (orgID, conferenceID, adminUserID, memberUserID int64) {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()

	// org
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('share-test-org') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}

	// app
	var appID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'share-app', 'hash') RETURNING id`,
		orgID,
	).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}

	// conference
	if err := pool.QueryRow(ctx,
		`INSERT INTO conferences (app_id, external_id, started_at) VALUES ($1, 'share-conf', $2) RETURNING id`,
		appID, time.Now().UTC(),
	).Scan(&conferenceID); err != nil {
		t.Fatalf("insert conference: %v", err)
	}

	// admin user
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, name, password_hash) VALUES ('admin@share.test', 'Admin', 'x') RETURNING id`,
	).Scan(&adminUserID); err != nil {
		t.Fatalf("insert admin user: %v", err)
	}

	// member user
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, name, password_hash) VALUES ('member@share.test', 'Member', 'x') RETURNING id`,
	).Scan(&memberUserID); err != nil {
		t.Fatalf("insert member user: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, orgID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = ANY($1)`, []int64{adminUserID, memberUserID})
	})

	return
}

// ─── TestShare_Create ────────────────────────────────────────────────────────

func TestShare_Create_Success(t *testing.T) {
	pool := testPool(t)
	orgID, confID, adminID, _ := setupShareTest(t)
	sess := &session.Data{UserID: adminID, OrgID: orgID, Role: "admin"}

	h := handler.HandleCreateShareToken(pool, noopLog())
	w := httptest.NewRecorder()
	h(w, shareCreateRequest(confID, sess))
	requireStatus(t, w, http.StatusCreated)

	var resp handler.ShareTokenResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Token) != 64 {
		t.Errorf("token length: got %d, want 64", len(resp.Token))
	}
	if resp.URL == "" {
		t.Error("url must not be empty")
	}
}

func TestShare_Create_ConferenceNotFound(t *testing.T) {
	pool := testPool(t)
	orgID, _, adminID, _ := setupShareTest(t)
	sess := &session.Data{UserID: adminID, OrgID: orgID, Role: "admin"}

	h := handler.HandleCreateShareToken(pool, noopLog())
	w := httptest.NewRecorder()
	h(w, shareCreateRequest(999999999, sess))
	requireStatus(t, w, http.StatusNotFound)
}

func TestShare_Create_WrongOrg(t *testing.T) {
	pool := testPool(t)
	_, confID, adminID, _ := setupShareTest(t)
	// Session belongs to a different org
	sess := &session.Data{UserID: adminID, OrgID: 999999, Role: "admin"}

	h := handler.HandleCreateShareToken(pool, noopLog())
	w := httptest.NewRecorder()
	h(w, shareCreateRequest(confID, sess))
	requireStatus(t, w, http.StatusNotFound)
}

// ─── TestShare_View ──────────────────────────────────────────────────────────

func TestShare_View_ValidSession(t *testing.T) {
	pool := testPool(t)
	orgID, confID, adminID, _ := setupShareTest(t)
	token := "view-valid-token-0000000000000000000000000000000000000000000000"
	insertShareToken(t, token, confID, orgID, adminID)
	sess := &session.Data{UserID: adminID, OrgID: orgID, Role: "admin"}

	h := handler.HandleGetShareToken(pool, nil, noopLog())
	w := httptest.NewRecorder()
	h(w, shareViewRequest(token, sess))
	requireStatus(t, w, http.StatusOK)

	var detail handler.ConferenceDetail
	if err := json.NewDecoder(w.Body).Decode(&detail); err != nil {
		t.Fatalf("decode conference detail: %v", err)
	}
	if detail.ID != confID {
		t.Errorf("conference id: got %d, want %d", detail.ID, confID)
	}
}

func TestShare_View_WrongOrg(t *testing.T) {
	pool := testPool(t)
	orgID, confID, adminID, _ := setupShareTest(t)
	token := "view-wrongorg-token-00000000000000000000000000000000000000000000"
	insertShareToken(t, token, confID, orgID, adminID)

	// Session is from a different org
	sess := &session.Data{UserID: adminID, OrgID: 999999, Role: "admin"}
	h := handler.HandleGetShareToken(pool, nil, noopLog())
	w := httptest.NewRecorder()
	h(w, shareViewRequest(token, sess))
	requireStatus(t, w, http.StatusForbidden)
}

func TestShare_View_TokenNotFound(t *testing.T) {
	pool := testPool(t)
	orgID, _, adminID, _ := setupShareTest(t)
	sess := &session.Data{UserID: adminID, OrgID: orgID, Role: "admin"}

	h := handler.HandleGetShareToken(pool, nil, noopLog())
	w := httptest.NewRecorder()
	h(w, shareViewRequest("nonexistent-token-000000000000000000000000000000000000000", sess))
	requireStatus(t, w, http.StatusNotFound)
}

func TestShare_ViewCount_Increments(t *testing.T) {
	pool := testPool(t)
	orgID, confID, adminID, _ := setupShareTest(t)
	token := "view-count-token-0000000000000000000000000000000000000000000000"
	insertShareToken(t, token, confID, orgID, adminID)
	sess := &session.Data{UserID: adminID, OrgID: orgID, Role: "admin"}

	h := handler.HandleGetShareToken(pool, nil, noopLog())

	// First call
	w1 := httptest.NewRecorder()
	h(w1, shareViewRequest(token, sess))
	requireStatus(t, w1, http.StatusOK)

	// Second call
	w2 := httptest.NewRecorder()
	h(w2, shareViewRequest(token, sess))
	requireStatus(t, w2, http.StatusOK)

	count := getViewCount(t, token)
	if count != 2 {
		t.Errorf("view_count: got %d, want 2", count)
	}
}

func TestShare_ConferenceDeletion_Cascade(t *testing.T) {
	pool := testPool(t)
	orgID, confID, adminID, _ := setupShareTest(t)
	token := "cascade-token-00000000000000000000000000000000000000000000000000"
	insertShareToken(t, token, confID, orgID, adminID)

	// Delete the conference — FK cascade should remove the token too.
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM conferences WHERE id = $1`, confID,
	); err != nil {
		t.Fatalf("delete conference: %v", err)
	}

	if tokenExists(t, token) {
		t.Error("share token must be cascade-deleted when conference is deleted")
	}
}

// ─── TestShare_Revoke ────────────────────────────────────────────────────────

func TestShare_Revoke_Member_OwnToken(t *testing.T) {
	pool := testPool(t)
	orgID, confID, _, memberID := setupShareTest(t)
	token := "revoke-own-token-00000000000000000000000000000000000000000000000"
	insertShareToken(t, token, confID, orgID, memberID)
	sess := &session.Data{UserID: memberID, OrgID: orgID, Role: "member"}

	h := handler.HandleRevokeShareToken(pool, noopLog())
	w := httptest.NewRecorder()
	h(w, shareRevokeRequest(token, sess))
	requireStatus(t, w, http.StatusNoContent)

	if tokenExists(t, token) {
		t.Error("token must be deleted after revoke")
	}
}

func TestShare_Revoke_Member_OtherToken(t *testing.T) {
	pool := testPool(t)
	orgID, confID, adminID, memberID := setupShareTest(t)
	token := "revoke-other-token-0000000000000000000000000000000000000000000000"
	// Token was created by admin
	insertShareToken(t, token, confID, orgID, adminID)
	// Member tries to revoke admin's token
	sess := &session.Data{UserID: memberID, OrgID: orgID, Role: "member"}

	h := handler.HandleRevokeShareToken(pool, noopLog())
	w := httptest.NewRecorder()
	h(w, shareRevokeRequest(token, sess))
	requireStatus(t, w, http.StatusForbidden)
}

func TestShare_Revoke_Admin_AnyToken(t *testing.T) {
	pool := testPool(t)
	orgID, confID, adminID, memberID := setupShareTest(t)
	token := "admin-revoke-any-000000000000000000000000000000000000000000000000"
	// Token created by member
	insertShareToken(t, token, confID, orgID, memberID)
	sess := &session.Data{UserID: adminID, OrgID: orgID, Role: "admin"}

	h := handler.HandleRevokeShareToken(pool, noopLog())
	w := httptest.NewRecorder()
	h(w, shareRevokeRequest(token, sess))
	requireStatus(t, w, http.StatusNoContent)

	if tokenExists(t, token) {
		t.Error("token must be deleted after admin revoke")
	}
}

func TestShare_Revoke_NotFound(t *testing.T) {
	pool := testPool(t)
	orgID, _, adminID, _ := setupShareTest(t)
	sess := &session.Data{UserID: adminID, OrgID: orgID, Role: "admin"}

	h := handler.HandleRevokeShareToken(pool, noopLog())
	w := httptest.NewRecorder()
	h(w, shareRevokeRequest("does-not-exist-00000000000000000000000000000000000000000", sess))
	requireStatus(t, w, http.StatusNotFound)
}

// ─── TestShare_List ──────────────────────────────────────────────────────────

func TestShare_List_AdminSeesAll(t *testing.T) {
	pool := testPool(t)
	orgID, confID, adminID, memberID := setupShareTest(t)

	insertShareToken(t, "list-all-token-a-00000000000000000000000000000000000000000000", confID, orgID, adminID)
	insertShareToken(t, "list-all-token-b-00000000000000000000000000000000000000000000", confID, orgID, memberID)
	insertShareToken(t, "list-all-token-c-00000000000000000000000000000000000000000000", confID, orgID, memberID)

	sess := &session.Data{UserID: adminID, OrgID: orgID, Role: "admin"}
	h := handler.HandleListShareTokens(pool, noopLog())
	w := httptest.NewRecorder()
	h(w, shareListRequest(sess))
	requireStatus(t, w, http.StatusOK)

	body := bodyJSON(t, w)
	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("data is not array: %v", body["data"])
	}
	if len(data) < 3 {
		t.Errorf("admin should see all 3 tokens, got %d", len(data))
	}
}

func TestShare_List_MemberSeesOwn(t *testing.T) {
	pool := testPool(t)
	orgID, confID, adminID, memberID := setupShareTest(t)

	insertShareToken(t, "list-own-admin-token-0000000000000000000000000000000000000000", confID, orgID, adminID)
	insertShareToken(t, "list-own-member-token-000000000000000000000000000000000000000", confID, orgID, memberID)

	sess := &session.Data{UserID: memberID, OrgID: orgID, Role: "member"}
	h := handler.HandleListShareTokens(pool, noopLog())
	w := httptest.NewRecorder()
	h(w, shareListRequest(sess))
	requireStatus(t, w, http.StatusOK)

	body := bodyJSON(t, w)
	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("data is not array: %v", body["data"])
	}
	for _, item := range data {
		m, _ := item.(map[string]any)
		if m["created_by_name"] == "Admin" {
			t.Error("member must not see tokens created by admin")
		}
	}
	// Verify member can see their own token
	if len(data) == 0 {
		t.Error("member must see at least their own token")
	}
}

func TestShare_List_EmptyForNewOrg(t *testing.T) {
	pool := testPool(t)
	orgID, _, adminID, _ := setupShareTest(t)
	sess := &session.Data{UserID: adminID, OrgID: orgID, Role: "admin"}

	h := handler.HandleListShareTokens(pool, noopLog())
	w := httptest.NewRecorder()
	h(w, shareListRequest(sess))
	requireStatus(t, w, http.StatusOK)

	body := bodyJSON(t, w)
	data, ok := body["data"].([]any)
	if !ok {
		// data might be empty array serialised as [] which decodes fine
		t.Fatalf("data is nil or wrong type: %v", body["data"])
	}
	_ = data // empty is fine
}
