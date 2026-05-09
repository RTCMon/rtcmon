package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
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
