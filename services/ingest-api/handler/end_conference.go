package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/auth"
)

// EMOSTriggerFn is called with the conference DB id after a successful end.
// Implementations must be non-blocking; the handler does not wait on them.
type EMOSTriggerFn func(conferenceID int64)

// resolveAppID returns the numeric app ID from whichever auth context is
// present in the request. JWT Claims take precedence; if AppID is non-empty
// it is parsed as int64. Falls back to ServerClaims for HMAC-authenticated
// routes. Returns a non-nil error when neither context is populated.
func resolveAppID(r *http.Request) (int64, error) {
	if claims := auth.ClaimsFromContext(r.Context()); claims.AppID != "" {
		return strconv.ParseInt(claims.AppID, 10, 64)
	}
	if sc := auth.ServerClaimsFromContext(r.Context()); sc != nil {
		return sc.AppID, nil
	}
	return 0, errors.New("no authentication claims in context")
}

// HandleEndConference implements POST /v1/conferences/{conferenceID}/end and
// POST /v1/server/conferences/{conferenceID}/end. The URL parameter
// {conferenceID} is the conference external_id (SDK-facing string identifier).
// The handler:
//  1. Resolves the caller's app ID from JWT Claims or ServerClaims
//  2. Looks up the conference by external_id (→ 404 if absent)
//  3. Verifies app ownership (→ 403 on mismatch)
//  4. Sets ended_at = now() idempotently (no-op if already ended)
//  5. Fires the eMOS trigger asynchronously (non-blocking)
//  6. Invalidates analytics cache keys for the app (fire-and-forget)
//  7. Returns 202
//
// @Summary End Conference
// @Description Ends a conference and triggers eMOS computation
// @Tags v1
// @Accept json
// @Produce json
// @Param conferenceID path string true "Conference ID"
// @Success 202 {object} map[string]interface{}
// @Failure 403 {object} map[string]interface{}
// @Failure 404 {object} map[string]interface{}
// @Router /v1/conferences/{conferenceID}/end [post]
func HandleEndConference(pool *pgxpool.Pool, rdb *redis.Client, log *logrus.Logger, triggerEMOS EMOSTriggerFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		externalID := chi.URLParam(r, "conferenceID")

		callerAppID, err := resolveAppID(r)
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

		if confAppID != callerAppID {
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

		// Invalidate analytics overview cache for this app (fire-and-forget).
		// The query-api stores keys as "overview:{appId}:{from}:{to}"; scanning
		// the prefix invalidates all time-range variants at once.
		if rdb != nil {
			go func() {
				ctx := context.Background()
				pattern := fmt.Sprintf("overview:%d:*", confAppID)
				iter := rdb.Scan(ctx, 0, pattern, 100).Iterator()
				for iter.Next(ctx) {
					rdb.Unlink(ctx, iter.Val())
				}
			}()
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
