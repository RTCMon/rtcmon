package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/RTCMon/rtcmon/internal/session"
	"github.com/RTCMon/rtcmon/services/query-api/handler"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

func newMiniredisStore(t *testing.T) (*miniredis.Miniredis, *session.Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, session.NewStore(rdb, 3600)
}

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// setupCleanUsers truncates the auth tables so register tests start from an
// empty state, and registers the same truncate as cleanup.
func setupCleanUsers(t *testing.T) {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx,
		`TRUNCATE organization_members, organizations, users RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate auth tables: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`TRUNCATE organization_members, organizations, users RESTART IDENTITY CASCADE`)
	})
}

// sessionCookie returns the value of the "session" Set-Cookie in a response,
// or "" if none.
func sessionCookie(w *httptest.ResponseRecorder) string {
	for _, raw := range w.Result().Cookies() {
		if raw.Name == "session" {
			return raw.Value
		}
	}
	return ""
}

// ─── unit tests (miniredis only, no DB) ──────────────────────────────────────

func TestMe_WithSession(t *testing.T) {
	sess := &session.Data{UserID: 7, OrgID: 3, Role: "admin", Email: "a@b.com", Name: "Alice"}
	ctx := session.WithContext(context.Background(), sess)

	h := handler.HandleMe()
	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h(w, req)

	requireStatus(t, w, http.StatusOK)
	body := bodyJSON(t, w)
	if body["email"] != "a@b.com" {
		t.Errorf("email: got %v", body["email"])
	}
	if body["role"] != "admin" {
		t.Errorf("role: got %v", body["role"])
	}
}

func TestMe_WithoutSession(t *testing.T) {
	h := handler.HandleMe()
	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	w := httptest.NewRecorder()
	h(w, req)
	requireStatus(t, w, http.StatusUnauthorized)
}

func TestLogout_NoCookieHandled(t *testing.T) {
	_, store := newMiniredisStore(t)
	h := handler.HandleLogout(store)
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	w := httptest.NewRecorder()
	h(w, req) // must not panic
	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", w.Code)
	}
}

// ─── integration tests (require TEST_DB_URL) ─────────────────────────────────

func TestRegister_FirstRun(t *testing.T) {
	setupCleanUsers(t)
	pool := testPool(t)
	_, store := newMiniredisStore(t)

	h := handler.HandleRegister(pool, store, noopLog())
	req := httptest.NewRequest(http.MethodPost, "/auth/register",
		jsonBody(map[string]string{"email": "admin@test.com", "password": "secret", "name": "Admin"}))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h(w, req)

	requireStatus(t, w, http.StatusCreated)

	body := bodyJSON(t, w)
	if body["role"] != "admin" {
		t.Errorf("role: got %v, want admin", body["role"])
	}
	if sessionCookie(w) == "" {
		t.Error("session cookie not set after register")
	}
}

func TestRegister_AlreadyExists(t *testing.T) {
	setupCleanUsers(t)
	pool := testPool(t)
	_, store := newMiniredisStore(t)

	h := handler.HandleRegister(pool, store, noopLog())

	// First registration.
	r1 := httptest.NewRequest(http.MethodPost, "/auth/register",
		jsonBody(map[string]string{"email": "admin@test.com", "password": "secret", "name": "Admin"}))
	r1.Header.Set("Content-Type", "application/json")
	h(httptest.NewRecorder(), r1)

	// Second registration must fail.
	r2 := httptest.NewRequest(http.MethodPost, "/auth/register",
		jsonBody(map[string]string{"email": "other@test.com", "password": "secret", "name": "Other"}))
	r2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	h(w2, r2)

	requireStatus(t, w2, http.StatusConflict)
	body := bodyJSON(t, w2)
	if body["error"] != "registration disabled" {
		t.Errorf("error: got %v", body["error"])
	}
}

func TestLogin_ValidCredentials(t *testing.T) {
	setupCleanUsers(t)
	pool := testPool(t)
	_, store := newMiniredisStore(t)

	// Register first.
	hReg := handler.HandleRegister(pool, store, noopLog())
	rReg := httptest.NewRequest(http.MethodPost, "/auth/register",
		jsonBody(map[string]string{"email": "user@test.com", "password": "mypass", "name": "User"}))
	rReg.Header.Set("Content-Type", "application/json")
	hReg(httptest.NewRecorder(), rReg)

	// Login.
	hLogin := handler.HandleLogin(pool, store, noopLog())
	rLogin := httptest.NewRequest(http.MethodPost, "/auth/login",
		jsonBody(map[string]string{"email": "user@test.com", "password": "mypass"}))
	rLogin.Header.Set("Content-Type", "application/json")
	wLogin := httptest.NewRecorder()
	hLogin(wLogin, rLogin)

	requireStatus(t, wLogin, http.StatusOK)
	if sessionCookie(wLogin) == "" {
		t.Error("session cookie not set after login")
	}
}

func TestLogin_InvalidPassword(t *testing.T) {
	setupCleanUsers(t)
	pool := testPool(t)
	_, store := newMiniredisStore(t)

	hReg := handler.HandleRegister(pool, store, noopLog())
	rReg := httptest.NewRequest(http.MethodPost, "/auth/register",
		jsonBody(map[string]string{"email": "user@test.com", "password": "correct", "name": "User"}))
	rReg.Header.Set("Content-Type", "application/json")
	hReg(httptest.NewRecorder(), rReg)

	hLogin := handler.HandleLogin(pool, store, noopLog())
	rLogin := httptest.NewRequest(http.MethodPost, "/auth/login",
		jsonBody(map[string]string{"email": "user@test.com", "password": "wrong"}))
	rLogin.Header.Set("Content-Type", "application/json")
	wLogin := httptest.NewRecorder()
	hLogin(wLogin, rLogin)

	requireStatus(t, wLogin, http.StatusUnauthorized)
	body := bodyJSON(t, wLogin)
	if body["error"] != "invalid credentials" {
		t.Errorf("error: got %v", body["error"])
	}
	if sessionCookie(wLogin) != "" {
		t.Error("session cookie must not be set on failed login")
	}
}

func TestLogin_UnknownEmail(t *testing.T) {
	setupCleanUsers(t)
	pool := testPool(t)
	_, store := newMiniredisStore(t)

	hLogin := handler.HandleLogin(pool, store, noopLog())
	rLogin := httptest.NewRequest(http.MethodPost, "/auth/login",
		jsonBody(map[string]string{"email": "nobody@test.com", "password": "anything"}))
	rLogin.Header.Set("Content-Type", "application/json")
	wLogin := httptest.NewRecorder()
	hLogin(wLogin, rLogin)

	requireStatus(t, wLogin, http.StatusUnauthorized)
	body := bodyJSON(t, wLogin)
	// Must be identical to wrong-password message — no email enumeration.
	if body["error"] != "invalid credentials" {
		t.Errorf("error: got %v", body["error"])
	}
}

func TestLogout_ClearsCookie(t *testing.T) {
	setupCleanUsers(t)
	pool := testPool(t)
	_, store := newMiniredisStore(t)

	// Register + login to get a token.
	hReg := handler.HandleRegister(pool, store, noopLog())
	rReg := httptest.NewRequest(http.MethodPost, "/auth/register",
		jsonBody(map[string]string{"email": "user@test.com", "password": "pass", "name": "User"}))
	rReg.Header.Set("Content-Type", "application/json")
	wReg := httptest.NewRecorder()
	hReg(wReg, rReg)
	token := sessionCookie(wReg)
	if token == "" {
		t.Fatal("no session cookie from register")
	}

	// Logout.
	hLogout := handler.HandleLogout(store)
	rLogout := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	rLogout.AddCookie(&http.Cookie{Name: "session", Value: token})
	wLogout := httptest.NewRecorder()
	hLogout(wLogout, rLogout)

	// The Set-Cookie header must expire the cookie (MaxAge < 0).
	var found bool
	for _, c := range wLogout.Result().Cookies() {
		if c.Name == "session" {
			found = true
			if c.MaxAge >= 0 {
				t.Errorf("session cookie MaxAge: got %d, want < 0", c.MaxAge)
			}
		}
	}
	if !found {
		t.Error("no session Set-Cookie in logout response")
	}

	// Token must be gone from the store.
	data, err := store.Get(context.Background(), token)
	if err != nil {
		t.Fatalf("store.Get after logout: %v", err)
	}
	if data != nil {
		t.Error("session still present in store after logout")
	}
}

func TestUpdateMe_Name(t *testing.T) {
	setupCleanUsers(t)
	pool := testPool(t)
	_, store := newMiniredisStore(t)

	hReg := handler.HandleRegister(pool, store, noopLog())
	rReg := httptest.NewRequest(http.MethodPost, "/auth/register",
		jsonBody(map[string]string{"email": "user@test.com", "password": "pass", "name": "OldName"}))
	rReg.Header.Set("Content-Type", "application/json")
	wReg := httptest.NewRecorder()
	hReg(wReg, rReg)

	sess := bodyJSON(t, wReg)
	userID := int64(sess["id"].(float64))

	newName := "NewName"
	ctx := session.WithContext(context.Background(), &session.Data{
		UserID: userID, OrgID: 1, Role: "admin", Email: "user@test.com", Name: "OldName",
	})

	hPatch := handler.HandlePatchMe(pool, store, noopLog())
	rPatch := httptest.NewRequest(http.MethodPatch, "/auth/me",
		jsonBody(map[string]any{"name": newName})).WithContext(ctx)
	rPatch.Header.Set("Content-Type", "application/json")
	wPatch := httptest.NewRecorder()
	hPatch(wPatch, rPatch)

	requireStatus(t, wPatch, http.StatusOK)

	// Verify name in DB.
	var storedName string
	if err := pool.QueryRow(context.Background(),
		`SELECT name FROM users WHERE id = $1`, userID,
	).Scan(&storedName); err != nil {
		t.Fatalf("query name: %v", err)
	}
	if storedName != newName {
		t.Errorf("name: got %q, want %q", storedName, newName)
	}
}

func TestUpdateMe_Password(t *testing.T) {
	setupCleanUsers(t)
	pool := testPool(t)
	_, store := newMiniredisStore(t)

	hReg := handler.HandleRegister(pool, store, noopLog())
	rReg := httptest.NewRequest(http.MethodPost, "/auth/register",
		jsonBody(map[string]string{"email": "user@test.com", "password": "oldpass", "name": "User"}))
	rReg.Header.Set("Content-Type", "application/json")
	wReg := httptest.NewRecorder()
	hReg(wReg, rReg)

	userID := int64(bodyJSON(t, wReg)["id"].(float64))

	newPass := "newpass123"
	ctx := session.WithContext(context.Background(), &session.Data{
		UserID: userID, OrgID: 1, Role: "admin", Email: "user@test.com", Name: "User",
	})

	hPatch := handler.HandlePatchMe(pool, store, noopLog())
	rPatch := httptest.NewRequest(http.MethodPatch, "/auth/me",
		jsonBody(map[string]any{"password": newPass})).WithContext(ctx)
	rPatch.Header.Set("Content-Type", "application/json")
	hPatch(httptest.NewRecorder(), rPatch)

	hLogin := handler.HandleLogin(pool, store, noopLog())

	// Old password must be rejected.
	rOld := httptest.NewRequest(http.MethodPost, "/auth/login",
		jsonBody(map[string]string{"email": "user@test.com", "password": "oldpass"}))
	rOld.Header.Set("Content-Type", "application/json")
	wOld := httptest.NewRecorder()
	hLogin(wOld, rOld)
	requireStatus(t, wOld, http.StatusUnauthorized)

	// New password must be accepted.
	rNew := httptest.NewRequest(http.MethodPost, "/auth/login",
		jsonBody(map[string]string{"email": "user@test.com", "password": newPass}))
	rNew.Header.Set("Content-Type", "application/json")
	wNew := httptest.NewRecorder()
	hLogin(wNew, rNew)
	requireStatus(t, wNew, http.StatusOK)
	if sessionCookie(wNew) == "" {
		t.Error("no session cookie on login with new password")
	}

	// Verify new password yields correct error message format on wrong attempt.
	rWrong := httptest.NewRequest(http.MethodPost, "/auth/login",
		jsonBody(map[string]string{"email": "user@test.com", "password": "stillwrong"}))
	rWrong.Header.Set("Content-Type", "application/json")
	wWrong := httptest.NewRecorder()
	hLogin(wWrong, rWrong)
	if !strings.Contains(wWrong.Body.String(), "invalid credentials") {
		t.Errorf("wrong error message: %s", wWrong.Body.String())
	}
}
