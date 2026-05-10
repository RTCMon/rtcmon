package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/session"
)

// ─── Types ───────────────────────────────────────────────────────────────────

// ShareTokenResponse is the JSON body for POST /v1/conferences/:id/share.
type ShareTokenResponse struct {
	Token string `json:"token"`
	URL   string `json:"url"`
}

// ShareTokenItem is one entry in the share token list response.
type ShareTokenItem struct {
	Token         string `json:"token"`
	ConferenceID  int64  `json:"conference_id"`
	ExternalID    string `json:"external_id"`
	CreatedAt     string `json:"created_at"` // RFC 3339
	ViewCount     int    `json:"view_count"`
	CreatedByName string `json:"created_by_name"`
}

// ─── Handlers ────────────────────────────────────────────────────────────────

// HandleCreateShareToken returns a handler for POST /v1/conferences/:id/share.
// Generates a share token for viewing the conference.
// Auth: Session required (any role).
func HandleCreateShareToken(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if sess == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		conferenceID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid conference ID"})
			return
		}

		// Verify conference exists and belongs to user's org
		var confOrgID int64
		err = db.QueryRow(r.Context(), `
			SELECT a.org_id FROM conferences c
			JOIN apps a ON a.id = c.app_id
			WHERE c.id = $1
		`, conferenceID).Scan(&confOrgID)
		if err == pgx.ErrNoRows {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "conference not found"})
			return
		}
		if err != nil {
			log.WithError(err).Error("create share token: query conference")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if confOrgID != sess.OrgID {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "conference not found"})
			return
		}

		// Generate 32-byte token (64-char hex)
		tokenBytes := make([]byte, 32)
		if _, err := rand.Read(tokenBytes); err != nil {
			log.WithError(err).Error("create share token: generate token")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		token := hex.EncodeToString(tokenBytes)

		// Insert into share_tokens
		_, err = db.Exec(r.Context(), `
			INSERT INTO share_tokens (token, conference_id, org_id, created_by)
			VALUES ($1, $2, $3, $4)
		`, token, conferenceID, sess.OrgID, sess.UserID)
		if err != nil {
			log.WithError(err).Error("create share token: insert")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		resp := ShareTokenResponse{
			Token: token,
			URL:   fmt.Sprintf("/share/%s", token),
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

// HandleGetShareToken returns a handler for GET /share/:token.
// Retrieves a shared conference detail. No session required, but redirects to login if absent.
// Requires session + org match.
func HandleGetShareToken(db *pgxpool.Pool, sessions *session.Store, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := chi.URLParam(r, "token")
		if token == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token required"})
			return
		}

		// Check if session exists
		sess := session.FromContext(r.Context())
		if sess == nil {
			// Redirect to login with next parameter
			loginURL := fmt.Sprintf("/login?next=%s", url.QueryEscape(r.RequestURI))
			http.Redirect(w, r, loginURL, http.StatusTemporaryRedirect)
			return
		}

		// Fetch share token and verify it exists
		var conferenceID, tokenOrgID int64
		err := db.QueryRow(r.Context(), `
			SELECT conference_id, org_id FROM share_tokens
			WHERE token = $1
		`, token).Scan(&conferenceID, &tokenOrgID)
		if err == pgx.ErrNoRows {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "share token not found"})
			return
		}
		if err != nil {
			log.WithError(err).Error("get share token: query share_tokens")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// Verify org match
		if tokenOrgID != sess.OrgID {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}

		// Increment view_count atomically
		_, err = db.Exec(r.Context(), `
			UPDATE share_tokens SET view_count = view_count + 1
			WHERE token = $1
		`, token)
		if err != nil {
			log.WithError(err).Error("get share token: increment view_count")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// Fetch conference detail (using existing helper) by delegating to HandleGetConference logic
		// Create a context with the conference ID in the chi params
		cCtx := context.WithValue(r.Context(), chi.RouteCtxKey, chi.NewRouteContext())
		chi.RouteContext(cCtx).URLParams.Add("conferenceId", strconv.FormatInt(conferenceID, 10))
		fakeReq := r.WithContext(cCtx)

		// Since HandleGetConference expects session in context, we'll copy that
		fakeReq = fakeReq.WithContext(session.WithContext(cCtx, sess))

		// We can't easily reuse HandleGetConference handler, so we'll fetch the detail here directly
		detail, err := fetchConferenceDetail(r.Context(), db, conferenceID, sess.OrgID)
		if err == pgx.ErrNoRows {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "conference not found"})
			return
		}
		if err != nil {
			log.WithError(err).Error("get share token: fetch conference detail")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		writeJSON(w, http.StatusOK, detail)
	}
}

// HandleRevokeShareToken returns a handler for DELETE /v1/share-tokens/:token.
// Revokes a share token. Admin can revoke any; Member can only revoke own.
func HandleRevokeShareToken(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if sess == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		token := chi.URLParam(r, "token")
		if token == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token required"})
			return
		}

		// Fetch token to get creator info
		var createdBy *int64
		err := db.QueryRow(r.Context(), `
			SELECT created_by FROM share_tokens
			WHERE token = $1 AND org_id = $2
		`, token, sess.OrgID).Scan(&createdBy)
		if err == pgx.ErrNoRows {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "share token not found"})
			return
		}
		if err != nil {
			log.WithError(err).Error("revoke share token: query")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// Check permissions: admin can revoke any, member can only revoke own
		if sess.Role != "admin" && (createdBy == nil || *createdBy != sess.UserID) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}

		// Delete the token
		result, err := db.Exec(r.Context(), `
			DELETE FROM share_tokens
			WHERE token = $1 AND org_id = $2
		`, token, sess.OrgID)
		if err != nil {
			log.WithError(err).Error("revoke share token: delete")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		if result.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "share token not found"})
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// HandleListShareTokens returns a handler for GET /v1/share-tokens.
// Lists share tokens for the org. Admin sees all; Member sees only own.
func HandleListShareTokens(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if sess == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		var query string
		var args []interface{}

		// Build query based on role
		if sess.Role == "admin" {
			// Admin sees all tokens in org
			query = `
				SELECT st.token, st.conference_id, c.external_id, st.created_at, st.view_count,
						COALESCE(u.name, 'unknown') as created_by_name
				FROM share_tokens st
				JOIN conferences c ON c.id = st.conference_id
				LEFT JOIN users u ON u.id = st.created_by
				WHERE st.org_id = $1
				ORDER BY st.created_at DESC
			`
			args = []interface{}{sess.OrgID}
		} else {
			// Member sees only own tokens
			query = `
				SELECT st.token, st.conference_id, c.external_id, st.created_at, st.view_count,
						COALESCE(u.name, 'unknown') as created_by_name
				FROM share_tokens st
				JOIN conferences c ON c.id = st.conference_id
				LEFT JOIN users u ON u.id = st.created_by
				WHERE st.org_id = $1 AND st.created_by = $2
				ORDER BY st.created_at DESC
			`
			args = []interface{}{sess.OrgID, sess.UserID}
		}

		rows, err := db.Query(r.Context(), query, args...)
		if err != nil {
			log.WithError(err).Error("list share tokens: query")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer rows.Close()

		var tokens []ShareTokenItem
		for rows.Next() {
			var item ShareTokenItem
			var createdAt interface{} // Will be time.Time, convert to RFC 3339
			if err := rows.Scan(&item.Token, &item.ConferenceID, &item.ExternalID, &createdAt, &item.ViewCount, &item.CreatedByName); err != nil {
				log.WithError(err).Error("list share tokens: scan")
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			// Format time as RFC 3339
			if t, ok := createdAt.(time.Time); ok {
				item.CreatedAt = t.Format(time.RFC3339)
			}
			tokens = append(tokens, item)
		}
		if err := rows.Err(); err != nil {
			log.WithError(err).Error("list share tokens: rows error")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		if tokens == nil {
			tokens = []ShareTokenItem{} // Return empty array, not null
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{"data": tokens})
	}
}

// ─── Helper Functions ────────────────────────────────────────────────────────

// fetchConferenceDetail fetches conference detail (same shape as BE-025).
// This is called internally for share token access.
func fetchConferenceDetail(ctx context.Context, db *pgxpool.Pool, conferenceID, orgID int64) (*ConferenceDetail, error) {
	var detail ConferenceDetail
	var confOrgID int64
	err := db.QueryRow(ctx, `
		SELECT c.id, c.external_id, c.started_at, c.ended_at, a.org_id
		FROM conferences c
		JOIN apps a ON a.id = c.app_id
		WHERE c.id = $1
	`, conferenceID).Scan(
		&detail.ID, &detail.ExternalID, &detail.StartedAt, &detail.EndedAt, &confOrgID,
	)
	if err != nil {
		return nil, err
	}
	if confOrgID != orgID {
		return nil, pgx.ErrNoRows // Return NoRows to indicate access denied
	}

	// Fetch participants
	detail.Participants = make([]ParticipantDetail, 0)
	participantRows, err := db.Query(ctx, `
		SELECT id, user_id, display_name, joined_at, left_at
		FROM participants
		WHERE conference_id = $1
		ORDER BY joined_at ASC
	`, conferenceID)
	if err != nil {
		return nil, err
	}
	defer participantRows.Close()

	participantOrder := make([]int64, 0)
	participantMap := make(map[int64]*ParticipantDetail)

	for participantRows.Next() {
		var p ParticipantDetail
		p.Sessions = make([]SessionDetail, 0)
		if err := participantRows.Scan(&p.ID, &p.UserID, &p.DisplayName, &p.JoinedAt, &p.LeftAt); err != nil {
			return nil, err
		}
		participantOrder = append(participantOrder, p.ID)
		participantMap[p.ID] = &p
	}
	if err := participantRows.Err(); err != nil {
		return nil, err
	}

	// Fetch sessions and connections if any participants
	if len(participantOrder) > 0 {
		sessionMap := make(map[int64]*SessionDetail)

		rows, err := db.Query(ctx, `
			SELECT
				s.id, s.participant_id, s.browser, s.os, s.country, s.city, s.network_type,
				c.id, c.peer_id, c.started_at, c.ended_at, c.ice_state, c.codec_audio, c.codec_video
			FROM sessions s
			LEFT JOIN connections c ON c.session_id = s.id
			WHERE s.participant_id = ANY($1)
			ORDER BY s.participant_id ASC, s.id ASC, c.started_at ASC NULLS LAST
		`, participantOrder)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		for rows.Next() {
			var (
				sID, pID                                int64
				browser, os, country, city, networkType string
				cID                                     *int64
				peerID                                  *string
				cStartedAt                              *time.Time
				cEndedAt                                *time.Time
				iceState                                *string
				codecAudio                              *string
				codecVideo                              *string
			)
			if err := rows.Scan(
				&sID, &pID, &browser, &os, &country, &city, &networkType,
				&cID, &peerID, &cStartedAt, &cEndedAt, &iceState, &codecAudio, &codecVideo,
			); err != nil {
				return nil, err
			}

			sd, seen := sessionMap[sID]
			if !seen {
				newSD := SessionDetail{
					ID:          sID,
					Browser:     browser,
					OS:          os,
					Country:     country,
					City:        city,
					NetworkType: networkType,
					Connections: make([]ConnectionDetail, 0),
				}
				p := participantMap[pID]
				p.Sessions = append(p.Sessions, newSD)
				sd = &p.Sessions[len(p.Sessions)-1]
				sessionMap[sID] = sd
			}

			if cID != nil {
				sd.Connections = append(sd.Connections, ConnectionDetail{
					ID:         *cID,
					PeerID:     *peerID,
					StartedAt:  *cStartedAt,
					EndedAt:    cEndedAt,
					IceState:   *iceState,
					CodecAudio: *codecAudio,
					CodecVideo: *codecVideo,
				})
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// Assemble final participants in order
	for _, pid := range participantOrder {
		detail.Participants = append(detail.Participants, *participantMap[pid])
	}

	return &detail, nil
}
