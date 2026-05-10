package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/session"
)

// ConferenceDetail is the full response for GET /v1/conferences/{conferenceId}.
type ConferenceDetail struct {
	ID           int64               `json:"id"`
	ExternalID   string              `json:"external_id"`
	StartedAt    time.Time           `json:"started_at"`
	EndedAt      *time.Time          `json:"ended_at"`
	Participants []ParticipantDetail `json:"participants"`
}

// ParticipantDetail is one participant with their sessions.
type ParticipantDetail struct {
	ID          int64           `json:"id"`
	UserID      string          `json:"user_id"`
	DisplayName string          `json:"display_name"`
	JoinedAt    time.Time       `json:"joined_at"`
	LeftAt      *time.Time      `json:"left_at"`
	Sessions    []SessionDetail `json:"sessions"`
}

// SessionDetail is one session with its connections.
type SessionDetail struct {
	ID          int64              `json:"id"`
	Browser     string             `json:"browser"`
	OS          string             `json:"os"`
	Country     string             `json:"country"`
	City        string             `json:"city"`
	NetworkType string             `json:"network_type"`
	Connections []ConnectionDetail `json:"connections"`
}

// ConnectionDetail is one WebRTC peer connection.
type ConnectionDetail struct {
	ID         int64      `json:"id"`
	PeerID     string     `json:"peer_id"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
	IceState   string     `json:"ice_state"`
	CodecAudio string     `json:"codec_audio"`
	CodecVideo string     `json:"codec_video"`
}

// HandleGetConference returns a handler for GET /v1/conferences/{conferenceId}.
// @Summary Get Conference
// @Description Get Conference endpoint
// @Tags v1
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /v1/conferences/{conferenceId} [get]
func HandleGetConference(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if sess == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		conferenceID, err := strconv.ParseInt(chi.URLParam(r, "conferenceId"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid conferenceId"})
			return
		}

		// Query 1: fetch conference + the owning app's org_id for auth.
		var detail ConferenceDetail
		var orgID int64
		err = db.QueryRow(r.Context(), `
			SELECT c.id, c.external_id, c.started_at, c.ended_at, a.org_id
			FROM conferences c
			JOIN apps a ON a.id = c.app_id
			WHERE c.id = $1
		`, conferenceID).Scan(
			&detail.ID, &detail.ExternalID, &detail.StartedAt, &detail.EndedAt, &orgID,
		)
		if err != nil {
			if err == pgx.ErrNoRows {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
				return
			}
			if log != nil {
				log.WithError(err).Error("conference detail: fetch conference")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if orgID != sess.OrgID {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}

		// Query 2: fetch participants ordered by joined_at.
		detail.Participants = make([]ParticipantDetail, 0)
		participantRows, err := db.Query(r.Context(), `
			SELECT id, user_id, display_name, joined_at, left_at
			FROM participants
			WHERE conference_id = $1
			ORDER BY joined_at ASC
		`, conferenceID)
		if err != nil {
			if log != nil {
				log.WithError(err).Error("conference detail: fetch participants")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer participantRows.Close()

		// Ordered participant IDs for preserving joined_at order in final result.
		participantOrder := make([]int64, 0)
		participantMap := make(map[int64]*ParticipantDetail)

		for participantRows.Next() {
			var p ParticipantDetail
			p.Sessions = make([]SessionDetail, 0)
			if err := participantRows.Scan(&p.ID, &p.UserID, &p.DisplayName, &p.JoinedAt, &p.LeftAt); err != nil {
				if log != nil {
					log.WithError(err).Error("conference detail: scan participant")
				}
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			participantOrder = append(participantOrder, p.ID)
			participantMap[p.ID] = &p
		}
		if err := participantRows.Err(); err != nil {
			if log != nil {
				log.WithError(err).Error("conference detail: participant rows error")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		participantRows.Close()

		// Query 3: fetch sessions + connections for all participants (skip if none).
		if len(participantOrder) > 0 {
			sessionMap := make(map[int64]*SessionDetail) // keyed by session ID

			rows, err := db.Query(r.Context(), `
				SELECT
					s.id, s.participant_id, s.browser, s.os, s.country, s.city, s.network_type,
					c.id, c.peer_id, c.started_at, c.ended_at, c.ice_state, c.codec_audio, c.codec_video
				FROM sessions s
				LEFT JOIN connections c ON c.session_id = s.id
				WHERE s.participant_id = ANY($1)
				ORDER BY s.participant_id ASC, s.id ASC, c.started_at ASC NULLS LAST
			`, participantOrder)
			if err != nil {
				if log != nil {
					log.WithError(err).Error("conference detail: fetch sessions")
				}
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			defer rows.Close()

			for rows.Next() {
				var (
					sID, pID    int64
					browser, os, country, city, networkType string
					// connection columns (nullable when LEFT JOIN yields no match)
					cID        *int64
					peerID     *string
					cStartedAt *time.Time
					cEndedAt   *time.Time
					iceState   *string
					codecAudio *string
					codecVideo *string
				)
				if err := rows.Scan(
					&sID, &pID, &browser, &os, &country, &city, &networkType,
					&cID, &peerID, &cStartedAt, &cEndedAt, &iceState, &codecAudio, &codecVideo,
				); err != nil {
					if log != nil {
						log.WithError(err).Error("conference detail: scan session row")
					}
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
					return
				}

				// Upsert session into participantMap.
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
					// Safe to take a pointer here: the SQL ORDER BY s.id ASC guarantees
					// all rows for this session arrive consecutively, so p.Sessions is
					// never appended to again while sd is in use. If the ORDER BY ever
					// changes, replace this with index-based access instead.
					sd = &p.Sessions[len(p.Sessions)-1]
					sessionMap[sID] = sd
				}

				// Append connection if the LEFT JOIN matched a row.
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
				if log != nil {
					log.WithError(err).Error("conference detail: session rows error")
				}
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
		}

		// Assemble final participants slice in joined_at order.
		for _, pid := range participantOrder {
			detail.Participants = append(detail.Participants, *participantMap[pid])
		}

		writeJSON(w, http.StatusOK, detail)
	}
}
