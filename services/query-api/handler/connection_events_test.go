package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/RTCMon/rtcmon/internal/session"
	"github.com/RTCMon/rtcmon/services/query-api/handler"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// eventsRequest builds a GET request to /v1/connections/{connectionId}/events
// with chi URL params and optional session injected into context.
func eventsRequest(connectionID int64, sess *session.Data) *http.Request {
	url := fmt.Sprintf("/v1/connections/%d/events", connectionID)
	r := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("connectionId", itoa(connectionID))
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

// eventsRequestRaw builds a request with a raw (possibly invalid) connectionId string.
func eventsRequestRaw(connectionID string, sess *session.Data) *http.Request {
	url := fmt.Sprintf("/v1/connections/%s/events", connectionID)
	r := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("connectionId", connectionID)
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

// setupEventsFixture creates org → app → conference → participant → session →
// connection and returns their IDs plus a session scoped to that org.
func setupEventsFixture(t *testing.T) (orgID, connID int64, sess *session.Data) {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()

	startedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('events-org') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}

	var appID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'events-app', 'hash') RETURNING id`,
		orgID,
	).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}

	var confID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO conferences (app_id, external_id, started_at) VALUES ($1, 'events-conf', $2) RETURNING id`,
		appID, startedAt,
	).Scan(&confID); err != nil {
		t.Fatalf("insert conference: %v", err)
	}

	var partID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO participants (conference_id, user_id, display_name) VALUES ($1, 'ev-u1', 'U1') RETURNING id`,
		confID,
	).Scan(&partID); err != nil {
		t.Fatalf("insert participant: %v", err)
	}

	var sessID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO sessions (participant_id) VALUES ($1) RETURNING id`,
		partID,
	).Scan(&sessID); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if err := pool.QueryRow(ctx,
		`INSERT INTO connections (session_id, peer_id, started_at, ice_state, codec_audio, codec_video)
		 VALUES ($1, 'peer', $2, 'connected', 'opus', 'vp8') RETURNING id`,
		sessID, startedAt,
	).Scan(&connID); err != nil {
		t.Fatalf("insert connection: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, orgID)
	})

	sess = &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}
	return
}

// insertEvent inserts one event row and returns its ID.
func insertEvent(t *testing.T, connID int64, ts time.Time, eventType string, payload string) int64 {
	t.Helper()
	pool := testPool(t)
	var id int64
	var err error
	if payload == "" {
		err = pool.QueryRow(context.Background(),
			`INSERT INTO events (connection_id, ts, event_type) VALUES ($1, $2, $3) RETURNING id`,
			connID, ts, eventType,
		).Scan(&id)
	} else {
		err = pool.QueryRow(context.Background(),
			`INSERT INTO events (connection_id, ts, event_type, payload) VALUES ($1, $2, $3, $4::jsonb) RETURNING id`,
			connID, ts, eventType, payload,
		).Scan(&id)
	}
	if err != nil {
		t.Fatalf("insert event %q: %v", eventType, err)
	}
	return id
}

// decodeEventsResponse decodes a ConnectionEventsResponse from the recorder body.
func decodeEventsResponse(t *testing.T, w *httptest.ResponseRecorder) handler.ConnectionEventsResponse {
	t.Helper()
	var resp handler.ConnectionEventsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode events response: %v (body=%q)", err, w.Body.String())
	}
	return resp
}

// ─── unit tests (no DB) ──────────────────────────────────────────────────────

func TestEvents_Unauthorized(t *testing.T) {
	h := handler.HandleGetConnectionEvents(nil, noopLog())
	r := eventsRequest(1, nil) // no session
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusUnauthorized)
}

func TestEvents_InvalidConnectionID(t *testing.T) {
	sess := &session.Data{UserID: 1, OrgID: 1, Role: "admin"}
	h := handler.HandleGetConnectionEvents(nil, noopLog())
	r := eventsRequestRaw("not-a-number", sess)
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusBadRequest)
}

// ─── integration tests (require TEST_DB_URL) ─────────────────────────────────

func TestEvents_NotFound(t *testing.T) {
	orgID, _, _ := setupEventsFixture(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	h := handler.HandleGetConnectionEvents(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, eventsRequest(999999999, sess))
	requireStatus(t, w, http.StatusNotFound)

	b := bodyJSON(t, w)
	if b["error"] != "not found" {
		t.Errorf("error: got %v", b["error"])
	}
}

func TestEvents_WrongApp(t *testing.T) {
	orgID1, _, _ := setupEventsFixture(t)
	_, connID2, _ := setupEventsFixture(t) // belongs to a different org

	sess := &session.Data{UserID: 1, OrgID: orgID1, Role: "admin"}

	h := handler.HandleGetConnectionEvents(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, eventsRequest(connID2, sess))
	requireStatus(t, w, http.StatusForbidden)
}

func TestEvents_Empty(t *testing.T) {
	_, connID, sess := setupEventsFixture(t)

	h := handler.HandleGetConnectionEvents(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, eventsRequest(connID, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeEventsResponse(t, w)
	if resp.Data == nil {
		t.Error("data must be [] not null")
	}
	if len(resp.Data) != 0 {
		t.Errorf("expected 0 events, got %d", len(resp.Data))
	}
}

func TestEvents_MixedTypes(t *testing.T) {
	_, connID, sess := setupEventsFixture(t)

	base := time.Now().UTC().Truncate(time.Second)
	insertEvent(t, connID, base, "ice_connection_state_change", `{"state":"connected"}`)
	insertEvent(t, connID, base.Add(time.Second), "mute", `{"track":"audio","muted":true}`)
	insertEvent(t, connID, base.Add(2*time.Second), "custom", `{"key":"value"}`)

	h := handler.HandleGetConnectionEvents(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, eventsRequest(connID, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeEventsResponse(t, w)
	if len(resp.Data) != 3 {
		t.Fatalf("expected 3 events, got %d", len(resp.Data))
	}

	wantTypes := []string{"ice_connection_state_change", "mute", "custom"}
	for i, ev := range resp.Data {
		if ev.EventType != wantTypes[i] {
			t.Errorf("event[%d].event_type: got %q, want %q", i, ev.EventType, wantTypes[i])
		}
		if ev.EventType == "" {
			t.Errorf("event[%d] has empty event_type", i)
		}
	}
}

func TestEvents_PayloadDeserialized(t *testing.T) {
	_, connID, sess := setupEventsFixture(t)

	base := time.Now().UTC().Truncate(time.Second)
	insertEvent(t, connID, base, "custom", `{"answer":42,"nested":{"ok":true}}`)

	h := handler.HandleGetConnectionEvents(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, eventsRequest(connID, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeEventsResponse(t, w)
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 event, got %d", len(resp.Data))
	}

	ev := resp.Data[0]
	if ev.Payload == nil {
		t.Fatal("payload must not be nil")
	}

	// Decode payload as a generic map to verify correct JSON round-trip.
	var m map[string]any
	if err := json.Unmarshal(*ev.Payload, &m); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if m["answer"] != float64(42) {
		t.Errorf("payload.answer: got %v, want 42", m["answer"])
	}
	nested, ok := m["nested"].(map[string]any)
	if !ok {
		t.Fatalf("payload.nested is not a map: %T", m["nested"])
	}
	if nested["ok"] != true {
		t.Errorf("payload.nested.ok: got %v, want true", nested["ok"])
	}
}

func TestEvents_NullPayload(t *testing.T) {
	_, connID, sess := setupEventsFixture(t)

	base := time.Now().UTC().Truncate(time.Second)
	// Insert event with no payload (NULL).
	insertEvent(t, connID, base, "ice_gathering_state_change", "")

	h := handler.HandleGetConnectionEvents(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, eventsRequest(connID, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeEventsResponse(t, w)
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 event, got %d", len(resp.Data))
	}

	// Payload should serialize to null (not omitted, not empty object).
	if resp.Data[0].Payload != nil {
		t.Errorf("payload: got %s, want null", *resp.Data[0].Payload)
	}

	// Verify JSON body contains "payload":null.
	if body := w.Body.String(); body == "" {
		t.Fatal("empty body")
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	dataArr, _ := raw["data"].([]any)
	if len(dataArr) == 0 {
		t.Fatal("data array empty")
	}
	firstEv, _ := dataArr[0].(map[string]any)
	payloadVal, exists := firstEv["payload"]
	if !exists {
		t.Error("payload key missing from JSON")
	}
	if payloadVal != nil {
		t.Errorf("payload: got %v, want null", payloadVal)
	}
}

func TestEvents_Ordered(t *testing.T) {
	_, connID, sess := setupEventsFixture(t)

	base := time.Now().UTC().Truncate(time.Second)
	// Insert 5 events in reverse order.
	for i := 4; i >= 0; i-- {
		insertEvent(t, connID, base.Add(time.Duration(i)*time.Second),
			fmt.Sprintf("event-%d", i), "")
	}

	h := handler.HandleGetConnectionEvents(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, eventsRequest(connID, sess))
	requireStatus(t, w, http.StatusOK)

	resp := decodeEventsResponse(t, w)
	if len(resp.Data) != 5 {
		t.Fatalf("expected 5 events, got %d", len(resp.Data))
	}

	// Verify ascending order.
	for i := 1; i < len(resp.Data); i++ {
		if !resp.Data[i].TS.After(resp.Data[i-1].TS) {
			t.Errorf("events not ordered by ts: row[%d]=%v row[%d]=%v",
				i-1, resp.Data[i-1].TS, i, resp.Data[i].TS)
		}
	}

	// Verify event_type corresponds to the expected order.
	for i, ev := range resp.Data {
		want := fmt.Sprintf("event-%d", i)
		if ev.EventType != want {
			t.Errorf("event[%d].event_type: got %q, want %q", i, ev.EventType, want)
		}
	}
}
