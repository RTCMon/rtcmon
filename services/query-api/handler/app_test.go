package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/RTCMon/rtcmon/internal/session"
	"github.com/RTCMon/rtcmon/services/query-api/handler"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// seedOrg inserts an org + makes userID an admin member; registers cleanup.
func seedOrg(t *testing.T, pool *pgxpool.Pool, userID int64) (orgID int64) {
	t.Helper()
	ctx := context.Background()

	err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('Test Org') RETURNING id`,
	).Scan(&orgID)
	if err != nil {
		t.Fatalf("seedOrg: insert org: %v", err)
	}

	_, err = pool.Exec(ctx,
		`INSERT INTO organization_members (org_id, user_id, role) VALUES ($1, $2, 'admin')`,
		orgID, userID,
	)
	if err != nil {
		t.Fatalf("seedOrg: insert member: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, orgID)
	})
	return orgID
}

// seedApp inserts an app in orgID with a generated api_key; returns appID + plaintext key.
func seedApp(t *testing.T, pool *pgxpool.Pool, orgID int64) (appID int64, apiKey string) {
	t.Helper()
	ctx := context.Background()

	apiKey = "test-api-key-from-seed-app-00000000000000000000000000000000"
	hash, err := bcrypt.GenerateFromPassword([]byte(apiKey), 4) // lower cost for testing
	if err != nil {
		t.Fatalf("seedApp: bcrypt: %v", err)
	}

	err = pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'Test App', $2) RETURNING id`,
		orgID, string(hash),
	).Scan(&appID)
	if err != nil {
		t.Fatalf("seedApp: insert app: %v", err)
	}

	return appID, apiKey
}

// withOrgSession builds a request with chi param {orgId} + session.
func withOrgSession(method, orgId string, sess *session.Data, body interface{}) *http.Request {
	var bodyReader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	} else {
		bodyReader = bytes.NewReader([]byte{})
	}

	r := httptest.NewRequest(method, "/", bodyReader)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("orgId", orgId)
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

// getAppHash queries apps.api_key_hash for an appID.
func getAppHash(t *testing.T, pool *pgxpool.Pool, appID int64) string {
	t.Helper()
	var hash string
	if err := pool.QueryRow(context.Background(), `SELECT api_key_hash FROM apps WHERE id = $1`, appID).Scan(&hash); err != nil {
		t.Fatalf("getAppHash: %v", err)
	}
	return hash
}

// ─── Tests ───────────────────────────────────────────────────────────────────

func TestCreateApp_ReturnsAPIKey(t *testing.T) {
	pool := testPool(t)
	orgID := seedOrg(t, pool, 1)
	sess := adminSession(orgID)

	reqBody := handler.CreateAppRequest{
		Name:          "New App",
		RetentionDays: 30,
	}
	r := withOrgSession(http.MethodPost, itoa(orgID), sess, reqBody)
	w := httptest.NewRecorder()

	h := handler.HandleCreateApp(pool, noopLog())
	h(w, r)
	requireStatus(t, w, http.StatusCreated)

	resp := bodyJSON(t, w)
	apiKey, ok := resp["api_key"].(string)
	if !ok || apiKey == "" {
		t.Fatalf("expected api_key in response, got %v", resp["api_key"])
	}

	// Verify GET list does NOT return the API key
	rList := withOrgSession(http.MethodGet, itoa(orgID), sess, nil)
	wList := httptest.NewRecorder()
	hList := handler.HandleListApps(pool, noopLog())
	hList(wList, rList)
	requireStatus(t, wList, http.StatusOK)

	listResp := bodyJSON(t, wList)
	data := listResp["data"].([]interface{})
	if len(data) != 1 {
		t.Fatalf("expected 1 app, got %d", len(data))
	}
	appItem := data[0].(map[string]interface{})
	if _, hasKey := appItem["api_key"]; hasKey {
		t.Fatalf("unexpected api_key found in list response")
	}
}

func TestCreateApp_WrongOrg(t *testing.T) {
	pool := testPool(t)
	orgID := seedOrg(t, pool, 1)
	// session is for a different org
	sess := &session.Data{UserID: 1, OrgID: 9999, Role: "admin"}

	reqBody := handler.CreateAppRequest{Name: "New App"}
	r := withOrgSession(http.MethodPost, itoa(orgID), sess, reqBody)
	w := httptest.NewRecorder()

	h := handler.HandleCreateApp(pool, noopLog())
	h(w, r)
	requireStatus(t, w, http.StatusForbidden)
}

func TestRotateKey_InvalidateAndValidKeys(t *testing.T) {
	pool := testPool(t)
	orgID := seedOrg(t, pool, 1)
	appID, oldKeyStr := seedApp(t, pool, orgID)
	sess := adminSession(orgID)

	// Rotate key
	r := withChiSession(http.MethodPost, itoa(orgID), itoa(appID), sess)
	w := httptest.NewRecorder()

	h := handler.HandleRotateKey(pool, noopLog())
	h(w, r)
	requireStatus(t, w, http.StatusOK)

	resp := bodyJSON(t, w)
	newKeyStr, ok := resp["api_key"].(string)
	if !ok || newKeyStr == "" {
		t.Fatalf("expected api_key in response, got %v", resp["api_key"])
	}

	newHash := getAppHash(t, pool, appID)

	// Old key should be invalid
	if err := bcrypt.CompareHashAndPassword([]byte(newHash), []byte(oldKeyStr)); err == nil {
		t.Fatalf("old key should not match the new hash")
	}

	// New key should be valid
	if err := bcrypt.CompareHashAndPassword([]byte(newHash), []byte(newKeyStr)); err != nil {
		t.Fatalf("new key should match the new hash: %v", err)
	}
}

func TestDeleteApp_CascadeScheduled(t *testing.T) {
	pool := testPool(t)
	orgID := seedOrg(t, pool, 1)
	appID, _ := seedApp(t, pool, orgID)
	sess := adminSession(orgID)

	r := withChiSession(http.MethodDelete, itoa(orgID), itoa(appID), sess)
	w := httptest.NewRecorder()

	h := handler.HandleDeleteApp(pool, noopLog())
	h(w, r)
	requireStatus(t, w, http.StatusAccepted) // 202

	// Wait briefly for the goroutine to finish executing the delete query
	time.Sleep(50 * time.Millisecond)

	// Verify app is gone
	var id int64
	err := pool.QueryRow(context.Background(), `SELECT id FROM apps WHERE id = $1`, appID).Scan(&id)
	if err == nil {
		t.Fatalf("app was not deleted")
	}
}
