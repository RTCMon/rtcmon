package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/auth"
	"github.com/RTCMon/rtcmon/internal/model"
	"github.com/RTCMon/rtcmon/internal/worker"
)

// HandleServerEvents handles POST /v1/server/events.
//
// Authentication is enforced upstream by auth.AuthenticateAPIKey; this handler
// trusts that ServerClaims are present in the context. Body validation reuses
// the same rules as HandleEvents. AppID is derived from the DB-resolved claims
// (not a JWT), and Source is always stamped as "server" for provenance.
func HandleServerEvents(log *logrus.Logger, enqueue EnqueueFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ServerClaimsFromContext(r.Context())
		if claims == nil {
			writeEventError(w, http.StatusUnauthorized, "missing authentication")
			return
		}

		log.WithField("app_id", claims.AppID).Debug("server_events: request received")

		var payload model.IngestPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeEventError(w, http.StatusBadRequest, "invalid JSON")
			return
		}

		if err := validatePayload(&payload); err != nil {
			writeEventError(w, http.StatusBadRequest, err.Error())
			return
		}

		payload.AppID = strconv.FormatInt(claims.AppID, 10)
		payload.Source = "server"

		if enqueue != nil {
			if err := enqueue(payload); err != nil {
				if errors.Is(err, worker.ErrChannelFull) {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(http.StatusTooManyRequests)
					body, _ := json.Marshal(map[string]string{"error": "service busy, retry later"})
					_, _ = w.Write(body)
					return
				}
				log.WithError(err).Warn("server_events: enqueue failed, accepting anyway")
			}
		}

		w.WriteHeader(http.StatusAccepted)
	}
}
