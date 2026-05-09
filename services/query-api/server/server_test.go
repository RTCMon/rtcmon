package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/session"
)

// newTestServer creates a Server with nil DB and Redis dependencies.
// Routes that do not ping those dependencies (middleware tests, health route
// registration checks) work safely. The Recoverer middleware catches any nil-
// pointer panics that would otherwise surface from the health handler.
func newTestServer() *Server {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return NewServer(context.Background(), nil, nil, log, nil, nil)
}

// newServerWithSessions starts a miniredis instance and returns a Server wired
// to it together with the miniredis handle (for time manipulation).
func newServerWithSessions(t *testing.T) (*Server, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	log := logrus.New()
	log.SetOutput(io.Discard)
	store := session.NewStore(rdb, 10) // 10-second TTL for tests
	return NewServer(context.Background(), nil, nil, log, store, nil), mr
}

func TestRequestID_Present(t *testing.T) {
	srv := newTestServer()

	// Register a trivial endpoint so the request goes through the full stack.
	srv.router.Get("/test-ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req1 := httptest.NewRequest(http.MethodGet, "/test-ping", nil)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, req1)

	id1 := w1.Header().Get("X-Request-ID")
	if id1 == "" {
		t.Fatal("X-Request-ID missing from first response")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/test-ping", nil)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)

	id2 := w2.Header().Get("X-Request-ID")
	if id2 == "" {
		t.Fatal("X-Request-ID missing from second response")
	}
	if id1 == id2 {
		t.Errorf("expected distinct request IDs; both are %q", id1)
	}
}

func TestPanicRecovery(t *testing.T) {
	srv := newTestServer()

	srv.router.Get("/ok", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv.router.Get("/panic", func(w http.ResponseWriter, r *http.Request) {
		panic("intentional panic for test")
	})

	// Normal request succeeds.
	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("normal request: got %d, want 200", w.Code)
	}

	// Panicking handler returns 500 instead of crashing.
	reqP := httptest.NewRequest(http.MethodGet, "/panic", nil)
	wP := httptest.NewRecorder()
	srv.ServeHTTP(wP, reqP)
	if wP.Code != http.StatusInternalServerError {
		t.Errorf("panic handler: got %d, want 500", wP.Code)
	}

	// Server still functional after panic.
	reqAfter := httptest.NewRequest(http.MethodGet, "/ok", nil)
	wAfter := httptest.NewRecorder()
	srv.ServeHTTP(wAfter, reqAfter)
	if wAfter.Code != http.StatusOK {
		t.Errorf("post-panic request: got %d, want 200", wAfter.Code)
	}
}

func TestHealthEndpoint_WhenCreated(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	// Health route must be registered — reject only 404.
	// With nil pool/redis, the pings panic and the recoverer returns 500;
	// that's still a valid registered route.
	if w.Code == http.StatusNotFound {
		t.Error("/health must be registered; got 404")
	}
}

func TestHealthRoute_NoAuth(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	// Health must never require authentication.
	if w.Code == http.StatusUnauthorized {
		t.Error("/health must not require auth; got 401")
	}
}

func TestLoggingMiddleware_IncludesRequestID(t *testing.T) {
	log := logrus.New()
	out := &strings.Builder{}
	log.SetOutput(out)
	log.SetFormatter(&logrus.JSONFormatter{})

	srv := NewServer(context.Background(), nil, nil, log, nil, nil)
	srv.router.Get("/log-test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/log-test", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	logStr := out.String()
	for _, field := range []string{"request_id", "method", "path", "status", "duration_ms"} {
		if !strings.Contains(logStr, field) {
			t.Errorf("log line missing field %q; log=%s", field, logStr)
		}
	}
}

func TestSession_Expiry(t *testing.T) {
	srv, mr := newServerWithSessions(t)

	// Create a session directly in the store.
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	store := session.NewStore(rdb, 10)
	token, err := store.Create(context.Background(), session.Data{
		UserID: 1, OrgID: 1, Role: "admin", Email: "x@test.com", Name: "X",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Request with valid session → not 401.
	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: token})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("expected non-401 with valid session, got 401")
	}

	// Advance miniredis clock past the 10-second TTL.
	mr.FastForward(11 * time.Second)

	// Same token now expired → 401.
	req2 := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req2.AddCookie(&http.Cookie{Name: "session", Value: token})
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("expired session: got %d, want 401", w2.Code)
	}
}

func TestCSRF_WrongOriginRejected(t *testing.T) {
	srv := newTestServer()
	srv.router.Post("/csrf-test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// POST with a mismatched Origin must be rejected with 403.
	req := httptest.NewRequest(http.MethodPost, "/csrf-test", nil)
	req.Host = "myapp.example.com"
	req.Header.Set("Origin", "http://evil.com")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("wrong origin: got %d, want 403", w.Code)
	}

	// POST without Origin header must pass through.
	req2 := httptest.NewRequest(http.MethodPost, "/csrf-test", nil)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("no origin: got %d, want 200", w2.Code)
	}

	// POST with correct Origin must pass through.
	req3 := httptest.NewRequest(http.MethodPost, "/csrf-test", nil)
	req3.Host = "example.com"
	req3.Header.Set("Origin", "http://example.com")
	w3 := httptest.NewRecorder()
	srv.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Errorf("correct origin: got %d, want 200", w3.Code)
	}
}
