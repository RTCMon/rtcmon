package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/session"
	"github.com/RTCMon/rtcmon/services/query-api/handler"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// noopLog returns a logrus.Logger that discards all output.
func noopLog() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(os.Stderr)
	l.SetLevel(logrus.PanicLevel)
	return l
}

// testPool opens a real pgxpool against TEST_DB_URL. The test is skipped when
// the variable is not set.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("TEST_DB_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// adminSession returns session Data for an admin in org orgID.
func adminSession(orgID int64) *session.Data {
	return &session.Data{UserID: 1, OrgID: orgID, Role: "admin", Email: "admin@test.com", Name: "Admin"}
}

// memberSession returns session Data for a member in org orgID.
func memberSession(orgID int64) *session.Data {
	return &session.Data{UserID: 2, OrgID: orgID, Role: "member", Email: "member@test.com", Name: "Member"}
}

// valid32ByteKey is a fixed AES master key used in tests.
var valid32ByteKey = []byte("12345678901234567890123456789012")

// withChiSession builds a request with chi URL params {orgId, appId} and a
// session in context. Pass nil for sess to simulate an unauthenticated request.
func withChiSession(method, orgId, appId string, sess *session.Data) *http.Request {
	r := httptest.NewRequest(method, "/", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("orgId", orgId)
	rctx.URLParams.Add("appId", appId)
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

func bodyJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("bodyJSON: %v (body=%q)", err, w.Body.String())
	}
	return m
}

func requireStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Errorf("HTTP status: got %d, want %d (body=%q)", w.Code, want, w.Body.String())
	}
}

// ─── unit tests (no DB needed) ───────────────────────────────────────────────

func TestServerKeyMissingMasterKey_Generate(t *testing.T) {
	h := handler.HandleGenerateServerKey(nil, nil, noopLog())
	r := withChiSession(http.MethodPost, "1", "1", adminSession(1))
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusServiceUnavailable)
	body := bodyJSON(t, w)
	if !strings.Contains(body["error"].(string), "not configured") {
		t.Errorf("unexpected error: %q", body["error"])
	}
}

func TestServerKeyMissingMasterKey_Rotate(t *testing.T) {
	h := handler.HandleRotateServerKey(nil, nil, noopLog())
	r := withChiSession(http.MethodPost, "1", "1", adminSession(1))
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusServiceUnavailable)
}

func TestServerKeyGenerate_Member(t *testing.T) {
	h := handler.HandleGenerateServerKey(nil, valid32ByteKey, noopLog())
	r := withChiSession(http.MethodPost, "1", "1", memberSession(1))
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusForbidden)
}

func TestServerKeyDelete_Member(t *testing.T) {
	h := handler.HandleDeleteServerKey(nil, noopLog())
	r := withChiSession(http.MethodDelete, "1", "1", memberSession(1))
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusForbidden)
}

func TestServerKeyRotate_Member(t *testing.T) {
	h := handler.HandleRotateServerKey(nil, valid32ByteKey, noopLog())
	r := withChiSession(http.MethodPost, "1", "1", memberSession(1))
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusForbidden)
}

func TestServerKeyWrongOrg(t *testing.T) {
	// Session belongs to org 99, but URL says org 1 → 403 on all four endpoints.
	wrongOrg := &session.Data{UserID: 3, OrgID: 99, Role: "admin"}

	cases := []struct {
		name string
		fn   http.HandlerFunc
	}{
		{"generate", handler.HandleGenerateServerKey(nil, valid32ByteKey, noopLog())},
		{"get", handler.HandleGetServerKey(nil, noopLog())},
		{"delete", handler.HandleDeleteServerKey(nil, noopLog())},
		{"rotate", handler.HandleRotateServerKey(nil, valid32ByteKey, noopLog())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := withChiSession(http.MethodPost, "1", "1", wrongOrg)
			w := httptest.NewRecorder()
			tc.fn(w, r)
			requireStatus(t, w, http.StatusForbidden)
		})
	}
}

func TestServerKey_Unauthorized_NoSession(t *testing.T) {
	cases := []struct {
		name string
		fn   http.HandlerFunc
	}{
		{"generate", handler.HandleGenerateServerKey(nil, valid32ByteKey, noopLog())},
		{"get", handler.HandleGetServerKey(nil, noopLog())},
		{"delete", handler.HandleDeleteServerKey(nil, noopLog())},
		{"rotate", handler.HandleRotateServerKey(nil, valid32ByteKey, noopLog())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := withChiSession(http.MethodPost, "1", "1", nil)
			w := httptest.NewRecorder()
			tc.fn(w, r)
			requireStatus(t, w, http.StatusUnauthorized)
		})
	}
}

// ─── integration tests (require TEST_DB_URL) ─────────────────────────────────

// setupApp creates a test org + app and registers cleanup. Returns orgID, appID.
func setupApp(t *testing.T, pool *pgxpool.Pool) (orgID, appID int64) {
	t.Helper()
	ctx := context.Background()

	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('test-org') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}

	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'test-app', 'hash') RETURNING id`,
		orgID,
	).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, orgID)
	})
	return
}

func TestServerKeyGenerate_Admin(t *testing.T) {
	pool := testPool(t)
	orgID, appID := setupApp(t, pool)
	sess := adminSession(orgID)

	h := handler.HandleGenerateServerKey(pool, valid32ByteKey, noopLog())
	r := withChiSession(http.MethodPost, itoa(orgID), itoa(appID), sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusCreated)

	body := bodyJSON(t, w)
	apiKey, _ := body["api_key"].(string)
	apiSecret, _ := body["api_secret"].(string)
	if len(apiKey) != 64 {
		t.Errorf("api_key length: got %d, want 64", len(apiKey))
	}
	if len(apiSecret) != 64 {
		t.Errorf("api_secret length: got %d, want 64", len(apiSecret))
	}
	if _, ok := body["created_at"]; !ok {
		t.Error("created_at missing from response")
	}
}

func TestServerKeyGenerate_AlreadyExists(t *testing.T) {
	pool := testPool(t)
	orgID, appID := setupApp(t, pool)
	sess := adminSession(orgID)
	h := handler.HandleGenerateServerKey(pool, valid32ByteKey, noopLog())

	// First call succeeds.
	r1 := withChiSession(http.MethodPost, itoa(orgID), itoa(appID), sess)
	w1 := httptest.NewRecorder()
	h(w1, r1)
	requireStatus(t, w1, http.StatusCreated)

	// Second call is 409.
	r2 := withChiSession(http.MethodPost, itoa(orgID), itoa(appID), sess)
	w2 := httptest.NewRecorder()
	h(w2, r2)
	requireStatus(t, w2, http.StatusConflict)

	body := bodyJSON(t, w2)
	if !strings.Contains(body["error"].(string), "already exists") {
		t.Errorf("unexpected error: %q", body["error"])
	}
}

func TestServerKeyGenerate_AppNotFound(t *testing.T) {
	pool := testPool(t)
	orgID, _ := setupApp(t, pool)
	sess := adminSession(orgID)

	h := handler.HandleGenerateServerKey(pool, valid32ByteKey, noopLog())
	r := withChiSession(http.MethodPost, itoa(orgID), "999999999", sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusNotFound)
}

func TestServerKeyGet_Exists(t *testing.T) {
	pool := testPool(t)
	orgID, appID := setupApp(t, pool)
	sess := adminSession(orgID)

	// Generate first.
	hGen := handler.HandleGenerateServerKey(pool, valid32ByteKey, noopLog())
	rGen := withChiSession(http.MethodPost, itoa(orgID), itoa(appID), sess)
	wGen := httptest.NewRecorder()
	hGen(wGen, rGen)
	requireStatus(t, wGen, http.StatusCreated)

	// GET — metadata only, no api_secret.
	hGet := handler.HandleGetServerKey(pool, noopLog())
	rGet := withChiSession(http.MethodGet, itoa(orgID), itoa(appID), sess)
	wGet := httptest.NewRecorder()
	hGet(wGet, rGet)
	requireStatus(t, wGet, http.StatusOK)

	body := bodyJSON(t, wGet)
	if _, hasSecret := body["api_secret"]; hasSecret {
		t.Error("api_secret must not appear in GET response")
	}
	if body["has_secret"] != true {
		t.Errorf("has_secret: got %v, want true", body["has_secret"])
	}
	if _, ok := body["api_key"]; !ok {
		t.Error("api_key missing from GET response")
	}
}

func TestServerKeyGet_NoKey(t *testing.T) {
	pool := testPool(t)
	orgID, appID := setupApp(t, pool)
	sess := adminSession(orgID)

	h := handler.HandleGetServerKey(pool, noopLog())
	r := withChiSession(http.MethodGet, itoa(orgID), itoa(appID), sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusNotFound)

	body := bodyJSON(t, w)
	if !strings.Contains(body["error"].(string), "no server key") {
		t.Errorf("unexpected error: %q", body["error"])
	}
}

func TestServerKeyDelete_Admin(t *testing.T) {
	pool := testPool(t)
	orgID, appID := setupApp(t, pool)
	sess := adminSession(orgID)

	// Generate then delete.
	hGen := handler.HandleGenerateServerKey(pool, valid32ByteKey, noopLog())
	rGen := withChiSession(http.MethodPost, itoa(orgID), itoa(appID), sess)
	wGen := httptest.NewRecorder()
	hGen(wGen, rGen)
	requireStatus(t, wGen, http.StatusCreated)

	hDel := handler.HandleDeleteServerKey(pool, noopLog())
	rDel := withChiSession(http.MethodDelete, itoa(orgID), itoa(appID), sess)
	wDel := httptest.NewRecorder()
	hDel(wDel, rDel)
	requireStatus(t, wDel, http.StatusNoContent)

	// Confirm DB columns are null.
	var key, enc *string
	_ = pool.QueryRow(context.Background(),
		`SELECT server_api_key, server_api_secret_enc FROM apps WHERE id = $1`, appID,
	).Scan(&key, &enc)
	if key != nil || enc != nil {
		t.Errorf("columns should be null after delete: key=%v enc=%v", key, enc)
	}
}

func TestServerKeyDelete_NoKey(t *testing.T) {
	pool := testPool(t)
	orgID, appID := setupApp(t, pool)
	sess := adminSession(orgID)

	h := handler.HandleDeleteServerKey(pool, noopLog())
	r := withChiSession(http.MethodDelete, itoa(orgID), itoa(appID), sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusNotFound)
}

func TestServerKeyRotate_Admin(t *testing.T) {
	pool := testPool(t)
	orgID, appID := setupApp(t, pool)
	sess := adminSession(orgID)

	// Generate initial key.
	hGen := handler.HandleGenerateServerKey(pool, valid32ByteKey, noopLog())
	rGen := withChiSession(http.MethodPost, itoa(orgID), itoa(appID), sess)
	wGen := httptest.NewRecorder()
	hGen(wGen, rGen)
	requireStatus(t, wGen, http.StatusCreated)
	oldKey := bodyJSON(t, wGen)["api_key"].(string)

	// Rotate.
	hRot := handler.HandleRotateServerKey(pool, valid32ByteKey, noopLog())
	rRot := withChiSession(http.MethodPost, itoa(orgID), itoa(appID), sess)
	wRot := httptest.NewRecorder()
	hRot(wRot, rRot)
	requireStatus(t, wRot, http.StatusOK)

	rotBody := bodyJSON(t, wRot)
	newKey := rotBody["api_key"].(string)
	newSecret := rotBody["api_secret"].(string)
	if len(newKey) != 64 {
		t.Errorf("new api_key length: got %d, want 64", len(newKey))
	}
	if len(newSecret) != 64 {
		t.Errorf("new api_secret length: got %d, want 64", len(newSecret))
	}
	if newKey == oldKey {
		t.Error("rotated key must differ from old key")
	}
}

func TestServerKeyRotate_NoKey(t *testing.T) {
	pool := testPool(t)
	orgID, appID := setupApp(t, pool)
	sess := adminSession(orgID)

	h := handler.HandleRotateServerKey(pool, valid32ByteKey, noopLog())
	r := withChiSession(http.MethodPost, itoa(orgID), itoa(appID), sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusNotFound)

	body := bodyJSON(t, w)
	if !strings.Contains(body["error"].(string), "no server key to rotate") {
		t.Errorf("unexpected error: %q", body["error"])
	}
}

func TestServerKeyRotate_AtomicSwap(t *testing.T) {
	pool := testPool(t)
	orgID, appID := setupApp(t, pool)
	sess := adminSession(orgID)

	hGen := handler.HandleGenerateServerKey(pool, valid32ByteKey, noopLog())
	rGen := withChiSession(http.MethodPost, itoa(orgID), itoa(appID), sess)
	wGen := httptest.NewRecorder()
	hGen(wGen, rGen)
	requireStatus(t, wGen, http.StatusCreated)
	oldKey := bodyJSON(t, wGen)["api_key"].(string)

	hRot := handler.HandleRotateServerKey(pool, valid32ByteKey, noopLog())
	rRot := withChiSession(http.MethodPost, itoa(orgID), itoa(appID), sess)
	wRot := httptest.NewRecorder()
	hRot(wRot, rRot)
	requireStatus(t, wRot, http.StatusOK)

	// Old key must no longer be in the DB.
	var storedKey *string
	_ = pool.QueryRow(context.Background(),
		`SELECT server_api_key FROM apps WHERE id = $1`, appID,
	).Scan(&storedKey)
	if storedKey != nil && *storedKey == oldKey {
		t.Error("old key still stored after rotate")
	}
}

func TestServerKeySecretNotInGet(t *testing.T) {
	pool := testPool(t)
	orgID, appID := setupApp(t, pool)
	sess := adminSession(orgID)

	hGen := handler.HandleGenerateServerKey(pool, valid32ByteKey, noopLog())
	rGen := withChiSession(http.MethodPost, itoa(orgID), itoa(appID), sess)
	wGen := httptest.NewRecorder()
	hGen(wGen, rGen)
	requireStatus(t, wGen, http.StatusCreated)

	hGet := handler.HandleGetServerKey(pool, noopLog())
	rGet := withChiSession(http.MethodGet, itoa(orgID), itoa(appID), sess)
	wGet := httptest.NewRecorder()
	hGet(wGet, rGet)
	requireStatus(t, wGet, http.StatusOK)

	if strings.Contains(wGet.Body.String(), "api_secret") {
		t.Errorf("GET response must not contain api_secret, got: %s", wGet.Body.String())
	}
}

// ─── utilities ───────────────────────────────────────────────────────────────

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
