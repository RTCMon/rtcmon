package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/auth"
	"github.com/RTCMon/rtcmon/internal/model"
)

const testSecret = "test-secret-key"

func newTestServer() *Server {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return NewServer(context.TODO(), nil, nil, log, testSecret, nil, nil, nil)
}

// makeValidToken generates a signed HS256 JWT with all required claims.
func makeValidToken(t *testing.T, secret string) string {
	t.Helper()
	claims := auth.Claims{}
	claims.AppID = "app-1"
	claims.ConferenceID = "conf-1"
	claims.UserID = "user-1"
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Hour))

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func validEventsBody(t *testing.T) *bytes.Buffer {
	t.Helper()
	p := model.IngestPayload{
		ConferenceID: "conf-1",
		SessionID:    "sess-1",
		ConnectionID: "conn-1",
		Events:       []model.StatSnapshot{{TS: 1_000_000}},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return bytes.NewBuffer(b)
}

// --- Auth wiring tests ---

func TestIngestRoute_NoAuth(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/events", validEventsBody(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

func TestIngestRoute_ValidAuth_ValidBody(t *testing.T) {
	srv := newTestServer()
	token := makeValidToken(t, testSecret)

	req := httptest.NewRequest(http.MethodPost, "/v1/events", validEventsBody(t))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("want 202, got %d; body: %s", w.Code, w.Body.String())
	}
}

func TestIngestRoute_ValidAuth_InvalidBody(t *testing.T) {
	srv := newTestServer()
	token := makeValidToken(t, testSecret)

	// Body is missing conference_id — should pass auth but fail validation.
	badPayload := model.IngestPayload{
		SessionID:    "sess-1",
		ConnectionID: "conn-1",
		Events:       []model.StatSnapshot{{TS: 1_000_000}},
	}
	b, _ := json.Marshal(badPayload)

	req := httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewBuffer(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d; body: %s", w.Code, w.Body.String())
	}
}

func TestHealthRoute_NoAuth(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	// Health endpoint is public — must not return 401.
	// It may return 503 because DB/Redis are nil, but never 401.
	if w.Code == http.StatusUnauthorized {
		t.Errorf("health endpoint must not require auth, got 401")
	}
	if w.Code == http.StatusNotFound {
		t.Errorf("health endpoint must be registered, got 404")
	}
}

// --- Original middleware tests (updated for new NewServer signature) ---

func TestRequestID_Present(t *testing.T) {
	srv := newTestServer()

	srv.router.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Header().Get("X-Request-ID") == "" {
		t.Error("expected X-Request-ID header to be present")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/test", nil)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)

	id1 := w.Header().Get("X-Request-ID")
	id2 := w2.Header().Get("X-Request-ID")
	if id1 == id2 {
		t.Errorf("expected different request IDs, got same: %s", id1)
	}
}

func TestPanicRecovery(t *testing.T) {
	srv := newTestServer()

	srv.router.Get("/ok", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv.router.Get("/panic", func(w http.ResponseWriter, r *http.Request) {
		panic("intentional panic")
	})

	reqOK := httptest.NewRequest(http.MethodGet, "/ok", nil)
	wOK := httptest.NewRecorder()
	srv.ServeHTTP(wOK, reqOK)
	if wOK.Code != http.StatusOK {
		t.Errorf("want 200 for normal request, got %d", wOK.Code)
	}

	reqPanic := httptest.NewRequest(http.MethodGet, "/panic", nil)
	wPanic := httptest.NewRecorder()
	srv.ServeHTTP(wPanic, reqPanic)
	if wPanic.Code != http.StatusInternalServerError {
		t.Errorf("want 500 for panic, got %d", wPanic.Code)
	}

	// Server still functional after panic.
	reqAfter := httptest.NewRequest(http.MethodGet, "/ok", nil)
	wAfter := httptest.NewRecorder()
	srv.ServeHTTP(wAfter, reqAfter)
	if wAfter.Code != http.StatusOK {
		t.Errorf("want 200 after panic recovery, got %d", wAfter.Code)
	}
}

func TestLoggingMiddleware_IncludesRequestID(t *testing.T) {
	log := logrus.New()
	out := &strings.Builder{}
	log.SetOutput(out)
	log.SetFormatter(&logrus.JSONFormatter{})

	srv := NewServer(context.TODO(), nil, nil, log, testSecret, nil, nil, nil)
	srv.router.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	logStr := out.String()
	for _, field := range []string{"request_id", "method", "path", "status", "duration_ms"} {
		if !strings.Contains(logStr, field) {
			t.Errorf("expected log to contain %q field", field)
		}
	}
}

func TestHealthEndpoint_WhenCreated(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code == http.StatusNotFound {
		t.Error("expected health endpoint to be registered, got 404")
	}
}
