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

func insertParticipant(t *testing.T, conferenceID int64, userID string, joinedAt time.Time) int64 {
	t.Helper()
	pool := testPool(t)
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO participants (conference_id, user_id, display_name, joined_at)
		 VALUES ($1, $2, $2, $3) RETURNING id`,
		conferenceID, userID, joinedAt,
	).Scan(&id); err != nil {
		t.Fatalf("insert participant %q: %v", userID, err)
	}
	return id
}

func insertSession(t *testing.T, participantID int64, browser, os string) int64 {
	t.Helper()
	pool := testPool(t)
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO sessions (participant_id, browser, os) VALUES ($1, $2, $3) RETURNING id`,
		participantID, browser, os,
	).Scan(&id); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return id
}

func insertConnection(t *testing.T, sessionID int64, peerID string, startedAt time.Time) int64 {
	t.Helper()
	pool := testPool(t)
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO connections (session_id, peer_id, started_at, ice_state, codec_audio, codec_video)
		 VALUES ($1, $2, $3, 'connected', 'opus', 'vp8') RETURNING id`,
		sessionID, peerID, startedAt,
	).Scan(&id); err != nil {
		t.Fatalf("insert connection: %v", err)
	}
	return id
}

func detailRequest(conferenceID int64, sess *session.Data) *http.Request {
	url := fmt.Sprintf("/v1/conferences/%d", conferenceID)
	r := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("conferenceId", itoa(conferenceID))
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if sess != nil {
		ctx = session.WithContext(ctx, sess)
	}
	return r.WithContext(ctx)
}

// ─── unit tests (no DB) ──────────────────────────────────────────────────────

func TestConferenceDetail_Unauthorized(t *testing.T) {
	h := handler.HandleGetConference(nil, noopLog())
	r := detailRequest(1, nil) // no session
	w := httptest.NewRecorder()
	h(w, r)
	requireStatus(t, w, http.StatusUnauthorized)
}

// ─── integration tests (require TEST_DB_URL) ─────────────────────────────────

func TestConferenceDetail_NotFound(t *testing.T) {
	orgID, _ := setupConferenceApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	h := handler.HandleGetConference(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, detailRequest(999999999, sess))
	requireStatus(t, w, http.StatusNotFound)

	b := bodyJSON(t, w)
	if b["error"] != "not found" {
		t.Errorf("error: got %v", b["error"])
	}
}

func TestConferenceDetail_WrongApp(t *testing.T) {
	// Two separate orgs+apps; session belongs to org1 but conference is in org2's app.
	orgID1, _ := setupConferenceApp(t)
	_, appID2 := setupConferenceApp(t)

	base := time.Now().UTC().Truncate(time.Second)
	confID := insertConference(t, appID2, "detail-wrong-app", base, nil)
	sess := &session.Data{UserID: 1, OrgID: orgID1, Role: "admin"}

	h := handler.HandleGetConference(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, detailRequest(confID, sess))
	requireStatus(t, w, http.StatusForbidden)
}

func TestConferenceDetail_EmptyParticipants(t *testing.T) {
	orgID, appID := setupConferenceApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	base := time.Now().UTC().Truncate(time.Second)
	confID := insertConference(t, appID, "detail-empty", base, nil)

	h := handler.HandleGetConference(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, detailRequest(confID, sess))
	requireStatus(t, w, http.StatusOK)

	var resp handler.ConferenceDetail
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Participants == nil {
		t.Error("participants must be [] not null")
	}
	if len(resp.Participants) != 0 {
		t.Errorf("participants: got %d, want 0", len(resp.Participants))
	}
}

func TestConferenceDetail_Valid(t *testing.T) {
	orgID, appID := setupConferenceApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	base := time.Now().UTC().Truncate(time.Second)
	confID := insertConference(t, appID, "detail-valid", base, nil)

	pID := insertParticipant(t, confID, "user-1", base.Add(time.Minute))
	sID := insertSession(t, pID, "Chrome", "macOS")
	cID := insertConnection(t, sID, "peer-x", base.Add(2*time.Minute))

	h := handler.HandleGetConference(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, detailRequest(confID, sess))
	requireStatus(t, w, http.StatusOK)

	var resp handler.ConferenceDetail
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.ID != confID {
		t.Errorf("id: got %d, want %d", resp.ID, confID)
	}
	if resp.ExternalID != "detail-valid" {
		t.Errorf("external_id: got %q", resp.ExternalID)
	}
	if len(resp.Participants) != 1 {
		t.Fatalf("participants: got %d, want 1", len(resp.Participants))
	}

	p := resp.Participants[0]
	if p.ID != pID || p.UserID != "user-1" {
		t.Errorf("participant: got id=%d user_id=%q", p.ID, p.UserID)
	}
	if len(p.Sessions) != 1 {
		t.Fatalf("sessions: got %d, want 1", len(p.Sessions))
	}

	s := p.Sessions[0]
	if s.ID != sID || s.Browser != "Chrome" || s.OS != "macOS" {
		t.Errorf("session: got id=%d browser=%q os=%q", s.ID, s.Browser, s.OS)
	}
	if len(s.Connections) != 1 {
		t.Fatalf("connections: got %d, want 1", len(s.Connections))
	}

	c := s.Connections[0]
	if c.ID != cID || c.PeerID != "peer-x" {
		t.Errorf("connection: got id=%d peer_id=%q", c.ID, c.PeerID)
	}
	if c.IceState != "connected" || c.CodecAudio != "opus" || c.CodecVideo != "vp8" {
		t.Errorf("connection fields: ice=%q audio=%q video=%q", c.IceState, c.CodecAudio, c.CodecVideo)
	}
}

func TestConferenceDetail_OrderedParticipants(t *testing.T) {
	orgID, appID := setupConferenceApp(t)
	sess := &session.Data{UserID: 1, OrgID: orgID, Role: "admin"}

	base := time.Now().UTC().Truncate(time.Second)
	confID := insertConference(t, appID, "detail-order", base, nil)

	// Insert in reverse order to verify DB ordering, not insertion order.
	insertParticipant(t, confID, "user-C", base.Add(30*time.Minute))
	insertParticipant(t, confID, "user-A", base.Add(10*time.Minute))
	insertParticipant(t, confID, "user-B", base.Add(20*time.Minute))

	h := handler.HandleGetConference(testPool(t), noopLog())
	w := httptest.NewRecorder()
	h(w, detailRequest(confID, sess))
	requireStatus(t, w, http.StatusOK)

	var resp handler.ConferenceDetail
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Participants) != 3 {
		t.Fatalf("participants: got %d, want 3", len(resp.Participants))
	}
	want := []string{"user-A", "user-B", "user-C"}
	for i, p := range resp.Participants {
		if p.UserID != want[i] {
			t.Errorf("participant[%d]: got %q, want %q", i, p.UserID, want[i])
		}
	}
}
