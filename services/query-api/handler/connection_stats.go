package handler

import (
	"context"
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

const statsCacheTTL = 60 * time.Second

// StatRow is one time-point in the connection stats time-series.
type StatRow struct {
	TS                 time.Time `json:"ts"`
	RTTMs              *float32  `json:"rtt_ms"`
	JitterMs           *float32  `json:"jitter_ms"`
	PacketLossRate     *float32  `json:"packet_loss_rate"`
	BitrateInKbps      *int32    `json:"bitrate_in_kbps"`
	BitrateOutKbps     *int32    `json:"bitrate_out_kbps"`
	EwmaRTTMs          *float32  `json:"ewma_rtt_ms"`
	EwmaJitterMs       *float32  `json:"ewma_jitter_ms"`
	EwmaPacketLossRate *float32  `json:"ewma_packet_loss_rate"`
	EwmaBitrateInKbps  *float32  `json:"ewma_bitrate_in_kbps"`
	EwmaBitrateOutKbps *float32  `json:"ewma_bitrate_out_kbps"`
	FPS                *int32    `json:"fps"`
	FrameWidth         *int32    `json:"frame_width"`
	FrameHeight        *int32    `json:"frame_height"`
	AudioLevel         *float32  `json:"audio_level"`
	ConcealmentRatio   *float32  `json:"concealment_ratio"`
	Source             string    `json:"source"`
}

// ConnectionStatsResponse is the JSON body for GET /v1/connections/{connectionId}/stats.
type ConnectionStatsResponse struct {
	Data []StatRow `json:"data"`
}

// HandleGetConnectionStats returns a handler for GET /v1/connections/{connectionId}/stats.
func HandleGetConnectionStats(db *pgxpool.Pool, rdb *redis.Client, log *logrus.Logger) http.HandlerFunc {
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

		q := r.URL.Query()

		var fromTime, toTime time.Time
		if s := q.Get("from"); s != "" {
			fromTime, err = time.Parse(time.RFC3339, s)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid from: must be RFC 3339"})
				return
			}
		}
		if s := q.Get("to"); s != "" {
			toTime, err = time.Parse(time.RFC3339, s)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid to: must be RFC 3339"})
				return
			}
		}

		sourceFilter := q.Get("source")
		switch sourceFilter {
		case "", "browser", "server":
			// valid
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source must be browser or server"})
			return
		}

		// Build cache key before touching the DB so a cache hit avoids all queries.
		sourcePart := sourceFilter
		if sourcePart == "" {
			sourcePart = "all"
		}
		fromPart := fromTime.UTC().Format(time.RFC3339)
		toPart := toTime.UTC().Format(time.RFC3339)
		cacheKey := fmt.Sprintf("stats:%d:%s:%s:%s", connectionID, fromPart, toPart, sourcePart)

		if rdb != nil {
			if cached, err := rdb.Get(r.Context(), cacheKey).Bytes(); err == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(cached)
				return
			}
		}

		// Fetch connection ownership + default time-range metadata in one query.
		var connStartedAt time.Time
		var connEndedAt *time.Time
		var orgID int64
		err = db.QueryRow(r.Context(), `
			SELECT c.started_at, c.ended_at, a.org_id
			FROM connections c
			JOIN sessions s     ON s.id = c.session_id
			JOIN participants p ON p.id = s.participant_id
			JOIN conferences cf ON cf.id = p.conference_id
			JOIN apps a         ON a.id = cf.app_id
			WHERE c.id = $1
		`, connectionID).Scan(&connStartedAt, &connEndedAt, &orgID)
		if err != nil {
			if err == pgx.ErrNoRows {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
				return
			}
			if log != nil {
				log.WithError(err).Error("connection stats: ownership check")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if orgID != sess.OrgID {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}

		// Apply default time range.
		if fromTime.IsZero() {
			fromTime = connStartedAt
		}
		if toTime.IsZero() {
			if connEndedAt != nil {
				toTime = *connEndedAt
			} else {
				toTime = connStartedAt.Add(time.Hour)
			}
		}

		// Fetch stats rows.
		rows, err := queryStats(r.Context(), db, connectionID, fromTime, toTime, sourceFilter)
		if err != nil {
			if log != nil {
				log.WithError(err).Error("connection stats: query")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		resp := ConnectionStatsResponse{Data: rows}

		encoded, err := json.Marshal(resp)
		if err != nil {
			if log != nil {
				log.WithError(err).Error("connection stats: marshal")
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// Populate cache — failure is non-fatal.
		if rdb != nil {
			if err := rdb.Set(r.Context(), cacheKey, encoded, statsCacheTTL).Err(); err != nil {
				if log != nil {
					log.WithError(err).Warn("connection stats: cache set failed")
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encoded)
	}
}

// queryStats executes the connection_stats SELECT and returns the scanned rows.
func queryStats(
	ctx context.Context,
	db *pgxpool.Pool,
	connectionID int64,
	from, to time.Time,
	sourceFilter string,
) ([]StatRow, error) {
	const baseSQL = `
SELECT ts,
       packets_lost_rate, jitter_ms, rtt_ms, bitrate_in_kbps, bitrate_out_kbps,
       ewma_packets_lost_rate, ewma_jitter_ms, ewma_rtt_ms,
       ewma_bitrate_in_kbps, ewma_bitrate_out_kbps,
       fps, frame_width, frame_height, audio_level, concealment_ratio, source
FROM connection_stats
WHERE connection_id = $1 AND ts >= $2 AND ts <= $3`

	var (
		sqlStr string
		args   []any
	)
	if sourceFilter != "" {
		sqlStr = baseSQL + " AND source = $4 ORDER BY ts ASC"
		args = []any{connectionID, from, to, sourceFilter}
	} else {
		sqlStr = baseSQL + " ORDER BY ts ASC"
		args = []any{connectionID, from, to}
	}

	pgRows, err := db.Query(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer pgRows.Close()

	result := make([]StatRow, 0)
	for pgRows.Next() {
		var row StatRow
		if err := pgRows.Scan(
			&row.TS,
			&row.PacketLossRate, &row.JitterMs, &row.RTTMs,
			&row.BitrateInKbps, &row.BitrateOutKbps,
			&row.EwmaPacketLossRate, &row.EwmaJitterMs, &row.EwmaRTTMs,
			&row.EwmaBitrateInKbps, &row.EwmaBitrateOutKbps,
			&row.FPS, &row.FrameWidth, &row.FrameHeight,
			&row.AudioLevel, &row.ConcealmentRatio,
			&row.Source,
		); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := pgRows.Err(); err != nil {
		return nil, err
	}

	return result, nil
}
