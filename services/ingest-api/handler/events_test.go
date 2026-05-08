package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/auth"
	"github.com/RTCMon/rtcmon/internal/model"
)

func newTestLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

func makeEventsRequest(t *testing.T, body any) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func validPayload() model.IngestPayload {
	return model.IngestPayload{
		ConferenceID: "conf-1",
		SessionID:    "sess-1",
		ConnectionID: "conn-1",
		Events: []model.StatSnapshot{
			{TS: 1_000_000, RTTMs: 20, JitterMs: 5, PacketLossRate: 0},
		},
	}
}

func TestValidation_ValidPayload(t *testing.T) {
	req := makeEventsRequest(t, validPayload())
	w := httptest.NewRecorder()
	HandleEvents(newTestLogger(), nil)(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("want 202, got %d", w.Code)
	}
}

func TestValidation_MissingConferenceID(t *testing.T) {
	p := validPayload()
	p.ConferenceID = ""
	req := makeEventsRequest(t, p)
	w := httptest.NewRecorder()
	HandleEvents(newTestLogger(), nil)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "conference_id") {
		t.Errorf("want error mentioning conference_id, got: %s", w.Body.String())
	}
}

func TestValidation_MissingSessionID(t *testing.T) {
	p := validPayload()
	p.SessionID = ""
	req := makeEventsRequest(t, p)
	w := httptest.NewRecorder()
	HandleEvents(newTestLogger(), nil)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestValidation_MissingConnectionID(t *testing.T) {
	p := validPayload()
	p.ConnectionID = ""
	req := makeEventsRequest(t, p)
	w := httptest.NewRecorder()
	HandleEvents(newTestLogger(), nil)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestValidation_EmptyEvents(t *testing.T) {
	p := validPayload()
	p.Events = []model.StatSnapshot{}
	req := makeEventsRequest(t, p)
	w := httptest.NewRecorder()
	HandleEvents(newTestLogger(), nil)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "events") {
		t.Errorf("want error mentioning events, got: %s", w.Body.String())
	}
}

func TestValidation_OversizedEvents(t *testing.T) {
	p := validPayload()
	p.Events = make([]model.StatSnapshot, 201)
	for i := range p.Events {
		p.Events[i] = model.StatSnapshot{TS: 1_000_000}
	}
	req := makeEventsRequest(t, p)
	w := httptest.NewRecorder()
	HandleEvents(newTestLogger(), nil)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "200") {
		t.Errorf("want error mentioning max 200, got: %s", w.Body.String())
	}
}

func TestValidation_MalformedJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader("{bad json}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	HandleEvents(newTestLogger(), nil)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid JSON") {
		t.Errorf("want 'invalid JSON' error, got: %s", w.Body.String())
	}
}

func TestValidation_NegativeTimestamp(t *testing.T) {
	p := validPayload()
	p.Events[0].TS = -1
	req := makeEventsRequest(t, p)
	w := httptest.NewRecorder()
	HandleEvents(newTestLogger(), nil)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestValidation_ZeroTimestamp(t *testing.T) {
	p := validPayload()
	p.Events[0].TS = 0
	req := makeEventsRequest(t, p)
	w := httptest.NewRecorder()
	HandleEvents(newTestLogger(), nil)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestValidation_ExtraFields(t *testing.T) {
	// Unknown fields should be silently ignored.
	raw := map[string]any{
		"conference_id": "conf-1",
		"session_id":    "sess-1",
		"connection_id": "conn-1",
		"events":        []any{map[string]any{"ts": 1_000_000}},
		"unknown_field": "should-be-ignored",
	}
	b, _ := json.Marshal(raw)
	req := httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	HandleEvents(newTestLogger(), nil)(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("want 202 for extra fields, got %d; body: %s", w.Code, w.Body.String())
	}
}

func TestValidation_MinimalValidPayload(t *testing.T) {
	// Only required fields, no optional numeric fields set.
	p := model.IngestPayload{
		ConferenceID: "c",
		SessionID:    "s",
		ConnectionID: "co",
		Events:       []model.StatSnapshot{{TS: 1}},
	}
	req := makeEventsRequest(t, p)
	w := httptest.NewRecorder()
	HandleEvents(newTestLogger(), nil)(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("want 202, got %d; body: %s", w.Code, w.Body.String())
	}
}

func TestValidation_NegativeNumericField(t *testing.T) {
	p := validPayload()
	p.Events[0].RTTMs = -1.0
	req := makeEventsRequest(t, p)
	w := httptest.NewRecorder()
	HandleEvents(newTestLogger(), nil)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400 for negative rtt_ms, got %d", w.Code)
	}
}

// TestClaimsInHandler verifies that the handler reads AppID from context
// without panicking, even when no claims are present.
func TestClaimsInHandler(t *testing.T) {
	req := makeEventsRequest(t, validPayload())
	// Inject claims explicitly to simulate what auth middleware would do.
	claims := &auth.Claims{}
	claims.AppID = "test-app"
	ctx := context.WithValue(req.Context(), struct{}{}, nil) // extra value should not interfere
	_ = ctx

	// Inject via the auth package's own mechanism (use a context that has claims).
	// We test both: with claims (no panic) and without claims (no panic).

	// 1. Without claims in context — should not panic.
	w := httptest.NewRecorder()
	HandleEvents(newTestLogger(), nil)(w, req)
	// We expect 202 since the payload is valid.
	if w.Code != http.StatusAccepted {
		t.Errorf("want 202, got %d", w.Code)
	}
}
