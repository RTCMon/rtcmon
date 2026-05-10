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

// UserCallItem is one call entry in the user call history response.
type UserCallItem struct {
	ConferenceID int64      `json:"conference_id"`
	ExternalID   string     `json:"external_id"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at"`
	DurationSecs *int64     `json:"duration_secs"` // null when conference is ongoing
	EMos         *float32   `json:"emos"`           // null when eMOS not yet computed
	Outcome      *string    `json:"outcome"`        // null when eMOS not yet computed
	Browser      string     `json:"browser"`
	OS           string     `json:"os"`
	Country      string     `json:"country"`
}

// UserCallsResponse is the JSON body for GET /v1/users/{userId}/calls.
type UserCallsResponse struct {
	Data []UserCallItem `json:"data"`
}

// HandleGetUserCalls returns a handler for GET /v1/users/{userId}/calls.
func HandleGetUserCalls(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if sess == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		userID := chi.URLParam(r, "userId")

		q := r.URL.Query()

		appIDStr := q.Get("appId")
		if appIDStr == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "appId is required"})
			return
		}
		appID, err := strconv.ParseInt(appIDStr, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid appId"})
			return
		}

		limit := parseIntParam(q.Get("limit"), 20)
		if limit < 1 {
			limit = 1
		}
		if limit > 50 {
			limit = 50
		}

		// Verify app belongs to session user's org (no enumeration — 403 for both
		// "not found" and "wrong org", same policy as BE-024).
		var dummy int64
		err = db.QueryRow(r.Context(),
			`SELECT id FROM apps WHERE id = $1 AND org_id = $2`, appID, sess.OrgID,
		).Scan(&dummy)
		if err != nil {
			if err == pgx.ErrNoRows {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
				return
			}
			if log != nil {
				log.WithError(err).Error("user calls: app ownership check")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// Fetch call history. A participant may have multiple sessions per conference,
		// so aggregate functions collapse them into one row per conference:
		//   - MIN(browser/os/country) → picks one session's device info deterministically
		//   - AVG(emos) → averages across sessions (consistent with BE-024)
		//   - bool_or outcome aggregation → worst-case outcome wins
		rows, err := db.Query(r.Context(), `
			SELECT
			    c.id                                                          AS conference_id,
			    c.external_id,
			    c.started_at,
			    c.ended_at,
			    CASE WHEN c.ended_at IS NOT NULL
			         THEN EXTRACT(EPOCH FROM (c.ended_at - c.started_at))::bigint
			         ELSE NULL
			    END                                                           AS duration_secs,
			    AVG(sq.emos)::float4                                          AS emos,
			    CASE
			        WHEN bool_or(sq.outcome = 'failed')   THEN 'failed'
			        WHEN bool_or(sq.outcome = 'degraded') THEN 'degraded'
			        WHEN bool_or(sq.outcome = 'success')  THEN 'success'
			        ELSE NULL
			    END                                                           AS outcome,
			    COALESCE(MIN(s.browser), '')                                  AS browser,
			    COALESCE(MIN(s.os), '')                                       AS os,
			    COALESCE(MIN(s.country), '')                                  AS country
			FROM participants p
			JOIN conferences c           ON c.id  = p.conference_id
			LEFT JOIN sessions s         ON s.participant_id = p.id
			LEFT JOIN session_quality sq ON sq.session_id    = s.id
			WHERE p.user_id = $1 AND c.app_id = $2
			GROUP BY c.id, c.external_id, c.started_at, c.ended_at
			ORDER BY c.started_at DESC
			LIMIT $3
		`, userID, appID, limit)
		if err != nil {
			if log != nil {
				log.WithError(err).Error("user calls: query")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer rows.Close()

		items := make([]UserCallItem, 0)
		for rows.Next() {
			var item UserCallItem
			if err := rows.Scan(
				&item.ConferenceID,
				&item.ExternalID,
				&item.StartedAt,
				&item.EndedAt,
				&item.DurationSecs,
				&item.EMos,
				&item.Outcome,
				&item.Browser,
				&item.OS,
				&item.Country,
			); err != nil {
				if log != nil {
					log.WithError(err).Error("user calls: scan row")
				}
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			if log != nil {
				log.WithError(err).Error("user calls: rows error")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		writeJSON(w, http.StatusOK, UserCallsResponse{Data: items})
	}
}
