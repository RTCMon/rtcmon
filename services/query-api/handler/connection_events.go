package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/session"
)

// EventRow is one event entry in the connection events response.
type EventRow struct {
	ID        int64            `json:"id"`
	TS        time.Time        `json:"ts"`
	EventType string           `json:"event_type"`
	Payload   *json.RawMessage `json:"payload"`
}

// ConnectionEventsResponse is the JSON body for GET /v1/connections/{connectionId}/events.
type ConnectionEventsResponse struct {
	Data []EventRow `json:"data"`
}

// HandleGetConnectionEvents returns a handler for GET /v1/connections/{connectionId}/events.
// @Summary Get Connection Events
// @Description Get Connection Events endpoint
// @Tags v1
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /v1/connections/{connectionId}/events [get]
func HandleGetConnectionEvents(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if sess == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		connectionID, err := strconv.ParseInt(chi.URLParam(r, "connectionId"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid connectionId"})
			return
		}

		// Verify ownership in a single round-trip.
		var orgID int64
		err = db.QueryRow(r.Context(), `
			SELECT a.org_id
			FROM connections c
			JOIN sessions s     ON s.id = c.session_id
			JOIN participants p ON p.id = s.participant_id
			JOIN conferences cf ON cf.id = p.conference_id
			JOIN apps a         ON a.id = cf.app_id
			WHERE c.id = $1
		`, connectionID).Scan(&orgID)
		if err != nil {
			if err == pgx.ErrNoRows {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
				return
			}
			if log != nil {
				log.WithError(err).Error("connection events: ownership check")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if orgID != sess.OrgID {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}

		// Fetch all events for the connection ordered by ts ascending.
		// The idx_events_connection_ts index on (connection_id, ts) makes this
		// an index scan with no additional sort step.
		rows, err := db.Query(r.Context(), `
			SELECT id, ts, event_type, payload
			FROM events
			WHERE connection_id = $1
			ORDER BY ts ASC
		`, connectionID)
		if err != nil {
			if log != nil {
				log.WithError(err).Error("connection events: query")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer rows.Close()

		events := make([]EventRow, 0)
		for rows.Next() {
			var ev EventRow
			if err := rows.Scan(&ev.ID, &ev.TS, &ev.EventType, &ev.Payload); err != nil {
				if log != nil {
					log.WithError(err).Error("connection events: scan row")
				}
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			events = append(events, ev)
		}
		if err := rows.Err(); err != nil {
			if log != nil {
				log.WithError(err).Error("connection events: rows error")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		writeJSON(w, http.StatusOK, ConnectionEventsResponse{Data: events})
	}
}
