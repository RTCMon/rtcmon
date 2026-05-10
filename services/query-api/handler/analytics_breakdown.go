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

const breakdownCacheTTL = 5 * time.Minute

// dimensionColumns maps the API `by` parameter to the sessions table column.
// Only values present in this map are accepted — any other value is rejected
// with 400 before the SQL is constructed, making the column interpolation safe.
var dimensionColumns = map[string]string{
	"browser":         "s.browser",
	"os":              "s.os",
	"region":          "s.country",
	"connection_type": "s.network_type",
}

// BreakdownItem is one row in the breakdown response.
type BreakdownItem struct {
	Value       string  `json:"value"`
	Count       int64   `json:"count"`
	AvgEMOS     float32 `json:"avg_emos"`
	SuccessRate float64 `json:"success_rate"`
}

// BreakdownResponse is the JSON body for GET /v1/apps/{appId}/analytics/breakdown.
type BreakdownResponse struct {
	Dimension string          `json:"dimension"`
	Data      []BreakdownItem `json:"data"`
}

// HandleGetAnalyticsBreakdown returns a handler for GET /v1/apps/{appId}/analytics/breakdown.
// @Summary Get Analytics Breakdown
// @Description Get Analytics Breakdown endpoint
// @Tags v1
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /v1/apps/{appId}/analytics/breakdown [get]
func HandleGetAnalyticsBreakdown(db *pgxpool.Pool, rdb *redis.Client, log *logrus.Logger) http.HandlerFunc {
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

		by := q.Get("by")
		if by == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "by is required"})
			return
		}
		col, ok := dimensionColumns[by]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid dimension"})
			return
		}

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

		cacheKey := fmt.Sprintf("breakdown:%d:%s:%s:%s",
			appID, by,
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

		// App ownership check — 403 for both not-found and wrong org.
		var dummy int64
		if err := db.QueryRow(r.Context(),
			`SELECT id FROM apps WHERE id = $1 AND org_id = $2`, appID, sess.OrgID,
		).Scan(&dummy); err != nil {
			if err == pgx.ErrNoRows {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
				return
			}
			if log != nil {
				log.WithError(err).Error("analytics breakdown: app ownership check")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// col is from the whitelist above — interpolation is injection-safe.
		sql := fmt.Sprintf(`
			SELECT
			    %s AS value,
			    COUNT(s.id) AS count,
			    COALESCE(AVG(sq.emos)::float4, 0) AS avg_emos,
			    COALESCE(
			        COUNT(CASE WHEN sq.outcome = 'success' THEN 1 END)::float8 /
			        NULLIF(COUNT(sq.session_id), 0),
			        0
			    ) AS success_rate
			FROM conferences c
			JOIN participants p    ON p.conference_id = c.id
			JOIN sessions s        ON s.participant_id = p.id
			LEFT JOIN session_quality sq ON sq.session_id = s.id
			WHERE c.app_id = $1 AND c.started_at >= $2 AND c.started_at < $3
			GROUP BY %s
			ORDER BY COUNT(s.id) DESC
		`, col, col)

		rows, err := db.Query(r.Context(), sql, appID, fromTime, toTime)
		if err != nil {
			if log != nil {
				log.WithError(err).Error("analytics breakdown: query")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer rows.Close()

		items := make([]BreakdownItem, 0)
		for rows.Next() {
			var item BreakdownItem
			if err := rows.Scan(&item.Value, &item.Count, &item.AvgEMOS, &item.SuccessRate); err != nil {
				if log != nil {
					log.WithError(err).Error("analytics breakdown: scan")
				}
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			if log != nil {
				log.WithError(err).Error("analytics breakdown: rows error")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		resp := BreakdownResponse{Dimension: by, Data: items}

		encoded, err := json.Marshal(resp)
		if err != nil {
			if log != nil {
				log.WithError(err).Error("analytics breakdown: marshal")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		if rdb != nil {
			if err := rdb.Set(r.Context(), cacheKey, encoded, breakdownCacheTTL).Err(); err != nil && log != nil {
				log.WithError(err).Warn("analytics breakdown: cache set failed")
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encoded)
	}
}
