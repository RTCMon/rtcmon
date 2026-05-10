package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/session"
)

const overviewCacheTTL = 5 * time.Minute

// OverviewResponse is the JSON body for GET /v1/apps/{appId}/analytics/overview.
type OverviewResponse struct {
	TotalConferences  int64   `json:"total_conferences"`
	CallSuccessRate   float64 `json:"call_success_rate"`
	AvgEMOS           float32 `json:"avg_emos"`
	P50SetupTimeMs    int64   `json:"p50_setup_time_ms"`
	P95SetupTimeMs    int64   `json:"p95_setup_time_ms"`
	TotalParticipants int64   `json:"total_participants"`
}

// HandleGetAnalyticsOverview returns a handler for GET /v1/apps/{appId}/analytics/overview.
func HandleGetAnalyticsOverview(db *pgxpool.Pool, rdb *redis.Client, log *logrus.Logger) http.HandlerFunc {
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

		q := r.URL.Query()

		fromStr := q.Get("from")
		if fromStr == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "from is required"})
			return
		}
		fromTime, err := time.Parse(time.RFC3339, fromStr)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid from: must be RFC 3339"})
			return
		}

		toStr := q.Get("to")
		if toStr == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "to is required"})
			return
		}
		toTime, err := time.Parse(time.RFC3339, toStr)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid to: must be RFC 3339"})
			return
		}

		// Cache key built before the ownership check so a warm cache avoids all DB work.
		cacheKey := fmt.Sprintf("overview:%d:%s:%s",
			appID,
			fromTime.UTC().Format(time.RFC3339),
			toTime.UTC().Format(time.RFC3339),
		)

		if rdb != nil {
			if cached, err := rdb.Get(r.Context(), cacheKey).Bytes(); err == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(cached)
				return
			}
		}

		// App ownership: same no-enumeration policy as other endpoints (403 for both
		// "not found" and "wrong org").
		var dummy int64
		if err := db.QueryRow(r.Context(),
			`SELECT id FROM apps WHERE id = $1 AND org_id = $2`, appID, sess.OrgID,
		).Scan(&dummy); err != nil {
			if err == pgx.ErrNoRows {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
				return
			}
			if log != nil {
				log.WithError(err).Error("analytics overview: app ownership check")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		var resp OverviewResponse
		if err := db.QueryRow(r.Context(), `
			SELECT
			    -- total conferences in range
			    (SELECT COUNT(*)
			     FROM conferences
			     WHERE app_id = $1 AND started_at >= $2 AND started_at < $3),

			    -- total distinct participants
			    (SELECT COUNT(DISTINCT p.id)
			     FROM conferences c
			     JOIN participants p ON p.conference_id = c.id
			     WHERE c.app_id = $1 AND c.started_at >= $2 AND c.started_at < $3),

			    -- average eMOS across all sessions with quality data
			    COALESCE(
			        (SELECT AVG(sq.emos)::float4
			         FROM conferences c
			         JOIN participants p    ON p.conference_id = c.id
			         JOIN sessions s        ON s.participant_id = p.id
			         JOIN session_quality sq ON sq.session_id   = s.id
			         WHERE c.app_id = $1 AND c.started_at >= $2 AND c.started_at < $3),
			        0),

			    -- call success rate: worst-case outcome per conference
			    --   numerator   = conferences whose worst outcome is 'success'
			    --   denominator = conferences that have any eMOS data (non-NULL outcome)
			    COALESCE(
			        (SELECT
			             COUNT(CASE WHEN conf_outcome = 'success' THEN 1 END)::float8 /
			             NULLIF(COUNT(conf_outcome), 0)
			         FROM (
			             SELECT CASE
			                 WHEN bool_or(sq.outcome = 'failed')   THEN 'failed'
			                 WHEN bool_or(sq.outcome = 'degraded') THEN 'degraded'
			                 WHEN bool_or(sq.outcome = 'success')  THEN 'success'
			                 ELSE NULL END AS conf_outcome
			             FROM conferences c
			             LEFT JOIN participants p    ON p.conference_id = c.id
			             LEFT JOIN sessions s        ON s.participant_id = p.id
			             LEFT JOIN session_quality sq ON sq.session_id   = s.id
			             WHERE c.app_id = $1 AND c.started_at >= $2 AND c.started_at < $3
			             GROUP BY c.id
			         ) outcomes),
			        0),

			    -- p50 connection setup time (ms): conn.started_at - participant.joined_at
			    COALESCE(
			        (SELECT (percentile_cont(0.5) WITHIN GROUP (
			                     ORDER BY EXTRACT(EPOCH FROM (conn.started_at - p.joined_at)) * 1000
			                 ))::bigint
			         FROM conferences c
			         JOIN participants p    ON p.conference_id = c.id
			         JOIN sessions s        ON s.participant_id = p.id
			         JOIN connections conn  ON conn.session_id  = s.id
			         WHERE c.app_id = $1 AND c.started_at >= $2 AND c.started_at < $3
			           AND conn.started_at >= p.joined_at),
			        0),

			    -- p95 connection setup time (ms)
			    COALESCE(
			        (SELECT (percentile_cont(0.95) WITHIN GROUP (
			                     ORDER BY EXTRACT(EPOCH FROM (conn.started_at - p.joined_at)) * 1000
			                 ))::bigint
			         FROM conferences c
			         JOIN participants p    ON p.conference_id = c.id
			         JOIN sessions s        ON s.participant_id = p.id
			         JOIN connections conn  ON conn.session_id  = s.id
			         WHERE c.app_id = $1 AND c.started_at >= $2 AND c.started_at < $3
			           AND conn.started_at >= p.joined_at),
			        0)
		`, appID, fromTime, toTime).Scan(
			&resp.TotalConferences,
			&resp.TotalParticipants,
			&resp.AvgEMOS,
			&resp.CallSuccessRate,
			&resp.P50SetupTimeMs,
			&resp.P95SetupTimeMs,
		); err != nil {
			if log != nil {
				log.WithError(err).Error("analytics overview: query")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		encoded, err := json.Marshal(resp)
		if err != nil {
			if log != nil {
				log.WithError(err).Error("analytics overview: marshal")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		if rdb != nil {
			if err := rdb.Set(r.Context(), cacheKey, encoded, overviewCacheTTL).Err(); err != nil && log != nil {
				log.WithError(err).Warn("analytics overview: cache set failed")
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encoded)
	}
}
