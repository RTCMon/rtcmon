package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/auth"
)

// EMOSTriggerFn is called with the conference DB id after a successful end.
// Implementations must be non-blocking; the handler does not wait on them.
type EMOSTriggerFn func(conferenceID int64)

// HandleEndConference implements POST /v1/conferences/{conferenceID}/end.
// The URL parameter {conferenceID} is the conference external_id (SDK-facing
// string identifier). The handler:
//  1. Looks up the conference by external_id (→ 404 if absent)
//  2. Verifies app ownership via JWT AppID (→ 403 on mismatch)
//  3. Sets ended_at = now() idempotently (no-op if already ended)
//  4. Fires the eMOS trigger asynchronously (non-blocking)
//  5. Returns 202
func HandleEndConference(pool *pgxpool.Pool, log *logrus.Logger, triggerEMOS EMOSTriggerFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		externalID := chi.URLParam(r, "conferenceID")
		claims := auth.ClaimsFromContext(r.Context())

		jwtAppID, err := strconv.ParseInt(claims.AppID, 10, 64)
		if err != nil {
			writeConferenceError(w, http.StatusUnauthorized, "invalid app_id in token")
			return
		}

		// Look up by external_id only so we can distinguish 404 from 403.
		var confDBID, confAppID int64
		err = pool.QueryRow(r.Context(),
			`SELECT id, app_id FROM conferences WHERE external_id = $1 LIMIT 1`,
			externalID,
		).Scan(&confDBID, &confAppID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeConferenceError(w, http.StatusNotFound, "conference not found")
				return
			}
			log.WithError(err).Error("end_conference: lookup failed")
			writeConferenceError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if confAppID != jwtAppID {
			writeConferenceError(w, http.StatusForbidden, "access denied")
			return
		}

		// Idempotent: only sets ended_at when it is not already set.
		if _, err = pool.Exec(r.Context(),
			`UPDATE conferences SET ended_at = now() WHERE id = $1 AND ended_at IS NULL`,
			confDBID,
		); err != nil {
			log.WithError(err).Error("end_conference: update failed")
			writeConferenceError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if triggerEMOS != nil {
			triggerEMOS(confDBID)
		}

		w.WriteHeader(http.StatusAccepted)
	}
}

func writeConferenceError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(body)
}
