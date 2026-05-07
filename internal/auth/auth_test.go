package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/RTCMon/rtcmon/internal/auth"
)

const testSecret = "super-secret-key-for-tests"

// makeToken builds a signed HS256 JWT with the provided claims.
// Pass a zero/negative duration for exp to produce an already-expired token.
func makeToken(t *testing.T, appID, confID, userID string, ttl time.Duration) string {
	t.Helper()

	claims := jwt.MapClaims{
		"app_id":        appID,
		"conference_id": confID,
		"user_id":       userID,
		"exp":           time.Now().Add(ttl).Unix(),
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("makeToken: %v", err)
	}
	return signed
}

// --- VerifyToken tests ---

func TestVerifyToken_Valid(t *testing.T) {
	token := makeToken(t, "app1", "conf1", "user1", time.Hour)

	claims, err := auth.VerifyToken(token, testSecret)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claims.AppID != "app1" {
		t.Errorf("AppID: got %q, want %q", claims.AppID, "app1")
	}
	if claims.ConferenceID != "conf1" {
		t.Errorf("ConferenceID: got %q, want %q", claims.ConferenceID, "conf1")
	}
	if claims.UserID != "user1" {
		t.Errorf("UserID: got %q, want %q", claims.UserID, "user1")
	}
}

func TestVerifyToken_Expired(t *testing.T) {
	token := makeToken(t, "app1", "conf1", "user1", -time.Minute)

	_, err := auth.VerifyToken(token, testSecret)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
	if !containsAny(err.Error(), "expired") {
		t.Errorf("error %q should mention 'expired'", err.Error())
	}
}

func TestVerifyToken_TamperedSignature(t *testing.T) {
	token := makeToken(t, "app1", "conf1", "user1", time.Hour)
	// Corrupt the signature portion of the JWT (last segment).
	tampered := token + "tampered"

	_, err := auth.VerifyToken(tampered, testSecret)
	if err == nil {
		t.Fatal("expected error for tampered token, got nil")
	}
}

func TestVerifyToken_MissingClaim_AppID(t *testing.T) {
	// Build a token without app_id.
	claims := jwt.MapClaims{
		"conference_id": "conf1",
		"user_id":       "user1",
		"exp":           time.Now().Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, _ := tok.SignedString([]byte(testSecret))

	_, err := auth.VerifyToken(signed, testSecret)
	if err == nil {
		t.Fatal("expected error for missing app_id, got nil")
	}
}

func TestVerifyToken_MissingClaim_UserID(t *testing.T) {
	// Build a token without user_id.
	claims := jwt.MapClaims{
		"app_id":        "app1",
		"conference_id": "conf1",
		"exp":           time.Now().Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, _ := tok.SignedString([]byte(testSecret))

	_, err := auth.VerifyToken(signed, testSecret)
	if err == nil {
		t.Fatal("expected error for missing user_id, got nil")
	}
}

// --- Middleware tests ---

// nextHandler is a simple handler that records whether it was called.
func nextHandler(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		w.WriteHeader(http.StatusOK)
	})
}

func TestMiddleware_ValidToken(t *testing.T) {
	token := makeToken(t, "app1", "conf1", "user1", time.Hour)

	var nextCalled bool
	var capturedClaims *auth.Claims

	handler := auth.Authenticate(testSecret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		capturedClaims = auth.ClaimsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if !nextCalled {
		t.Fatal("next handler was not called for valid token")
	}
	if capturedClaims == nil {
		t.Fatal("claims are nil in context")
	}
	if capturedClaims.AppID != "app1" {
		t.Errorf("AppID in context: got %q, want %q", capturedClaims.AppID, "app1")
	}
}

func TestMiddleware_NoHeader(t *testing.T) {
	var nextCalled bool
	handler := auth.Authenticate(testSecret)(nextHandler(&nextCalled))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if nextCalled {
		t.Error("next handler should NOT be called when no Authorization header")
	}
	assertAuthError(t, rr, http.StatusUnauthorized, "missing token")
}

func TestMiddleware_InvalidToken(t *testing.T) {
	var nextCalled bool
	handler := auth.Authenticate(testSecret)(nextHandler(&nextCalled))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer not.a.valid.jwt")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if nextCalled {
		t.Error("next handler should NOT be called for invalid token")
	}
	assertAuthError(t, rr, http.StatusUnauthorized, "invalid token")
}

func TestMiddleware_BareToken_NoPrefix(t *testing.T) {
	token := makeToken(t, "app1", "conf1", "user1", time.Hour)

	var nextCalled bool
	handler := auth.Authenticate(testSecret)(nextHandler(&nextCalled))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	// No "Bearer " prefix — should be rejected.
	req.Header.Set("Authorization", token)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if nextCalled {
		t.Error("next handler should NOT be called for bare token without Bearer prefix")
	}
	assertAuthError(t, rr, http.StatusUnauthorized, "missing token")
}

// --- ClaimsFromContext tests ---

func TestClaimsFromContext_NilContext(t *testing.T) {
	// Must not panic.
	claims := auth.ClaimsFromContext(nil) //nolint:staticcheck
	if claims == nil {
		t.Error("ClaimsFromContext(nil) returned nil; want zero-value Claims")
	}
}

func TestClaimsFromContext_EmptyContext(t *testing.T) {
	claims := auth.ClaimsFromContext(context.Background())
	if claims == nil {
		t.Error("ClaimsFromContext(empty) returned nil; want zero-value Claims")
	}
	if claims.AppID != "" {
		t.Errorf("AppID: got %q, want empty string", claims.AppID)
	}
}

// --- Helpers ---

func assertAuthError(t *testing.T, rr *httptest.ResponseRecorder, status int, wantMsg string) {
	t.Helper()
	if rr.Code != status {
		t.Errorf("status: got %d, want %d", rr.Code, status)
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v — body: %s", err, rr.Body.String())
	}
	if body["error"] != wantMsg {
		t.Errorf("error body: got %q, want %q", body["error"], wantMsg)
	}
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if len(sub) > 0 && contains(s, sub) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
