package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/session"
)

// ConferenceItem is one entry in the conference list response.
type ConferenceItem struct {
	ID               int64      `json:"id"`
	ExternalID       string     `json:"external_id"`
	StartedAt        time.Time  `json:"started_at"`
	EndedAt          *time.Time `json:"ended_at"`
	ParticipantCount int        `json:"participant_count"`
	EMos             *float32   `json:"emos"`
	Outcome          *string    `json:"outcome"`
}

// PaginationMeta carries page/limit/total for list responses.
type PaginationMeta struct {
	Page  int `json:"page"`
	Limit int `json:"limit"`
	Total int `json:"total"`
}

// ConferenceListResponse is the JSON body for GET /v1/apps/{appId}/conferences.
type ConferenceListResponse struct {
	Data       []ConferenceItem `json:"data"`
	Pagination PaginationMeta   `json:"pagination"`
}

// qualityToOutcome maps the human-facing quality param to the DB outcome value.
var qualityToOutcome = map[string]string{
	"good":     "success",
	"degraded": "degraded",
	"failed":   "failed",
}

// HandleListConferences returns a handler for GET /v1/apps/{appId}/conferences.
func HandleListConferences(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if sess == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		appID, err := strconv.ParseInt(chi.URLParam(r, "appId"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid appId"})
			return
		}

		// Parse and validate query params before hitting the DB.
		q := r.URL.Query()

		page := parseIntParam(q.Get("page"), 1)
		if page < 1 {
			page = 1
		}
		limit := parseIntParam(q.Get("limit"), 20)
		if limit < 1 {
			limit = 1
		}
		if limit > 100 {
			limit = 100
		}
		offset := (page - 1) * limit

		var fromTime, toTime *time.Time
		if s := q.Get("from"); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid from: must be RFC 3339"})
				return
			}
			fromTime = &t
		}
		if s := q.Get("to"); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid to: must be RFC 3339"})
				return
			}
			toTime = &t
		}

		var outcomeFilter *string
		if s := q.Get("quality"); s != "" {
			outcome, ok := qualityToOutcome[s]
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "quality must be good, degraded, or failed"})
				return
			}
			outcomeFilter = &outcome
		}

		// Verify the app belongs to the session user's org. Treats "not found"
		// and "wrong org" identically (no enumeration).
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
				log.WithError(err).Error("conference list: app ownership check")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// Build the shared CTE + WHERE clause used by both count and data queries.
		const cte = `
WITH conf_quality AS (
    SELECT
        p.conference_id,
        AVG(sq.emos)::float4 AS emos,
        CASE
            WHEN bool_or(sq.outcome = 'failed')   THEN 'failed'
            WHEN bool_or(sq.outcome = 'degraded') THEN 'degraded'
            WHEN bool_or(sq.outcome = 'success')  THEN 'success'
            ELSE NULL
        END AS outcome
    FROM participants p
    JOIN sessions s          ON s.participant_id = p.id
    LEFT JOIN session_quality sq ON sq.session_id = s.id
    GROUP BY p.conference_id
)`

		args := []any{appID}
		argN := 2 // next placeholder index
		where := "WHERE c.app_id = $1"

		if fromTime != nil {
			where += fmt.Sprintf(" AND c.started_at >= $%d", argN)
			args = append(args, *fromTime)
			argN++
		}
		if toTime != nil {
			where += fmt.Sprintf(" AND c.started_at <= $%d", argN)
			args = append(args, *toTime)
			argN++
		}
		if outcomeFilter != nil {
			where += fmt.Sprintf(" AND cq.outcome = $%d", argN)
			args = append(args, *outcomeFilter)
			argN++
		}

		// Count query.
		countSQL := cte + `
SELECT COUNT(*)
FROM conferences c
LEFT JOIN conf_quality cq ON cq.conference_id = c.id
` + where

		var total int
		if err := db.QueryRow(r.Context(), countSQL, args...).Scan(&total); err != nil {
			if log != nil {
				log.WithError(err).Error("conference list: count query")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// Data query.
		dataSQL := cte + `
SELECT c.id, c.external_id, c.started_at, c.ended_at,
       c.participant_count, cq.emos, cq.outcome
FROM conferences c
LEFT JOIN conf_quality cq ON cq.conference_id = c.id
` + where + fmt.Sprintf(`
ORDER BY c.started_at DESC
LIMIT $%d OFFSET $%d`, argN, argN+1)

		dataArgs := append(args, limit, offset)

		rows, err := db.Query(r.Context(), dataSQL, dataArgs...)
		if err != nil {
			if log != nil {
				log.WithError(err).Error("conference list: data query")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer rows.Close()

		items := make([]ConferenceItem, 0)
		for rows.Next() {
			var item ConferenceItem
			if err := rows.Scan(
				&item.ID, &item.ExternalID, &item.StartedAt, &item.EndedAt,
				&item.ParticipantCount, &item.EMos, &item.Outcome,
			); err != nil {
				if log != nil {
					log.WithError(err).Error("conference list: scan row")
				}
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			if log != nil {
				log.WithError(err).Error("conference list: rows error")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		writeJSON(w, http.StatusOK, ConferenceListResponse{
			Data: items,
			Pagination: PaginationMeta{
				Page:  page,
				Limit: limit,
				Total: total,
			},
		})
	}
}

func parseIntParam(s string, defaultVal int) int {
	if s == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	return n
}
