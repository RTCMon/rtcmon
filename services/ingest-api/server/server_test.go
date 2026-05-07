package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

// TestRequestID_Present verifies that every request gets a unique X-Request-ID header
func TestRequestID_Present(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	// Create a test server with a new router
	srv := NewServer(nil, nil, nil, log)

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()

	// Use the router directly with test endpoint
	testRouter := srv.router
	testRouter.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"message": "ok"})
	})

	srv.ServeHTTP(w, req)

	requestID := w.Header().Get("X-Request-ID")
	if requestID == "" {
		t.Errorf("expected X-Request-ID header to be present")
	}

	// Make another request and verify it has a different ID
	req2 := httptest.NewRequest("GET", "/test", nil)
	w2 := httptest.NewRecorder()

	srv.ServeHTTP(w2, req2)

	requestID2 := w2.Header().Get("X-Request-ID")
	if requestID2 == "" {
		t.Errorf("expected X-Request-ID header to be present on second request")
	}

	if requestID == requestID2 {
		t.Errorf("expected different request IDs, got same: %s", requestID)
	}
}

// TestPanicRecovery verifies that panics are caught and return 500
func TestPanicRecovery(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	srv := NewServer(nil, nil, nil, log)

	// Add test endpoints
	srv.router.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"message": "ok"})
	})

	srv.router.Get("/panic", func(w http.ResponseWriter, r *http.Request) {
		panic("intentional panic for testing")
	})

	// First, make a normal request
	req1 := httptest.NewRequest("GET", "/test", nil)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, req1)

	if w1.Code != http.StatusOK {
		t.Errorf("expected normal request to return 200, got %d", w1.Code)
	}

	// Now make a request that will panic
	req2 := httptest.NewRequest("GET", "/panic", nil)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)

	if w2.Code != http.StatusInternalServerError {
		t.Errorf("expected panic to return 500, got %d", w2.Code)
	}

	// Verify the process didn't crash and we can still serve requests
	req3 := httptest.NewRequest("GET", "/test", nil)
	w3 := httptest.NewRecorder()
	srv.ServeHTTP(w3, req3)

	if w3.Code != http.StatusOK {
		t.Errorf("expected normal request after panic to return 200, got %d", w3.Code)
	}
}

// TestLoggingMiddleware_IncludesRequestID verifies that logging includes request ID
func TestLoggingMiddleware_IncludesRequestID(t *testing.T) {
	log := logrus.New()

	// Capture log output
	logOutput := &strings.Builder{}
	log.SetOutput(logOutput)
	log.SetFormatter(&logrus.JSONFormatter{})

	srv := NewServer(nil, nil, nil, log)

	srv.router.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"message": "ok"})
	})

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	// Check that the log contains the request ID and other fields
	logStr := logOutput.String()
	if !strings.Contains(logStr, "request_id") {
		t.Errorf("expected log to contain request_id field")
	}
	if !strings.Contains(logStr, "method") {
		t.Errorf("expected log to contain method field")
	}
	if !strings.Contains(logStr, "path") {
		t.Errorf("expected log to contain path field")
	}
	if !strings.Contains(logStr, "status") {
		t.Errorf("expected log to contain status field")
	}
	if !strings.Contains(logStr, "duration_ms") {
		t.Errorf("expected log to contain duration_ms field")
	}
}

// TestHealthEndpoint_WhenCreated verifies the health endpoint is registered
func TestHealthEndpoint_WhenCreated(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	srv := NewServer(nil, nil, nil, log)

	// Verify health endpoint exists (we can't easily call it without real DB/Redis,
	// but we can verify the route was registered)
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	// The endpoint will fail because we have nil db/redis, but that's expected
	// We're just verifying the route is registered (status won't be 404)
	if w.Code == http.StatusNotFound {
		t.Errorf("expected health endpoint to be registered, got 404")
	}
}
