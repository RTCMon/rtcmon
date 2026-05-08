package handler_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/auth"
	"github.com/RTCMon/rtcmon/internal/model"
	"github.com/RTCMon/rtcmon/internal/worker"
	"github.com/RTCMon/rtcmon/services/ingest-api/handler"
)

func newServerEventsLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

// validServerPayload returns a minimal valid IngestPayload body.
func validServerPayload(t *testing.T) *bytes.Buffer {
	t.Helper()
	p := model.IngestPayload{
		ConferenceID: "conf-1",
		SessionID:    "sess-1",
		ConnectionID: "conn-1",
		Events:       []model.StatSnapshot{{TS: 1_000_000}},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bytes.NewBuffer(b)
}

// TestServerEventsHandler_NoClaims verifies that a request without ServerClaims
// in context (middleware not wired) is rejected with 401.
func TestServerEventsHandler_NoClaims(t *testing.T) {
	h := handler.HandleServerEvents(newServerEventsLogger(), nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/server/events", validServerPayload(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

// TestServerEventsHandler_InvalidBody verifies that validation runs after auth.
func TestServerEventsHandler_InvalidBody(t *testing.T) {
	h := handler.HandleServerEvents(newServerEventsLogger(), nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/server/events",
		bytes.NewBufferString(`{"session_id":"s1","connection_id":"c1","events":[{"ts":1}]}`))
	req = auth.WithServerClaims(req, &auth.ServerClaims{AppID: 1, OrgID: 1, APIKey: "k"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// Missing conference_id → 400.
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d; body: %s", w.Code, w.Body.String())
	}
}

// TestServerEventsHandler_Valid verifies a well-formed request returns 202.
func TestServerEventsHandler_Valid(t *testing.T) {
	h := handler.HandleServerEvents(newServerEventsLogger(), nil /* enqueue=nil, skip enqueue */)

	req := httptest.NewRequest(http.MethodPost, "/v1/server/events", validServerPayload(t))
	req = auth.WithServerClaims(req, &auth.ServerClaims{AppID: 42, OrgID: 7, APIKey: "k"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("want 202, got %d; body: %s", w.Code, w.Body.String())
	}
}

// TestServerEventsHandler_EnqueueFull verifies that a full worker channel returns 429.
func TestServerEventsHandler_EnqueueFull(t *testing.T) {
	enqueueFull := func(_ model.IngestPayload) error {
		return worker.ErrChannelFull
	}
	h := handler.HandleServerEvents(newServerEventsLogger(), enqueueFull)

	req := httptest.NewRequest(http.MethodPost, "/v1/server/events", validServerPayload(t))
	req = auth.WithServerClaims(req, &auth.ServerClaims{AppID: 1, OrgID: 1, APIKey: "k"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("want 429, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") != "1" {
		t.Errorf("want Retry-After: 1, got %q", w.Header().Get("Retry-After"))
	}
}

// TestServerEventsHandler_SourceField verifies that the enqueued payload has
// Source="server" regardless of what the client sent.
func TestServerEventsHandler_SourceField(t *testing.T) {
	var got model.IngestPayload
	capture := func(p model.IngestPayload) error {
		got = p
		return nil
	}
	h := handler.HandleServerEvents(newServerEventsLogger(), capture)

	req := httptest.NewRequest(http.MethodPost, "/v1/server/events", validServerPayload(t))
	req = auth.WithServerClaims(req, &auth.ServerClaims{AppID: 99, OrgID: 3, APIKey: "k"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", w.Code)
	}
	if got.Source != "server" {
		t.Errorf("Source = %q, want %q", got.Source, "server")
	}
}

// TestServerEventsHandler_AppIDFromClaims verifies that AppID in the enqueued
// payload comes from ServerClaims, not the request body.
func TestServerEventsHandler_AppIDFromClaims(t *testing.T) {
	var got model.IngestPayload
	capture := func(p model.IngestPayload) error {
		got = p
		return nil
	}
	h := handler.HandleServerEvents(newServerEventsLogger(), capture)

	req := httptest.NewRequest(http.MethodPost, "/v1/server/events", validServerPayload(t))
	req = auth.WithServerClaims(req, &auth.ServerClaims{AppID: 55, OrgID: 2, APIKey: "k"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", w.Code)
	}
	if got.AppID != "55" {
		t.Errorf("AppID = %q, want %q", got.AppID, "55")
	}
}
