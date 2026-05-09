package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/cache"
	"github.com/RTCMon/rtcmon/internal/db"
)

// HealthResponse is the JSON body returned by GET /health.
type HealthResponse struct {
	Status string `json:"status"`
	DB     string `json:"db"`
	Redis  string `json:"redis"`
}

// HandleHealth returns a handler that pings Postgres and Redis and reports
// their status. Returns 200 when both are reachable, 503 otherwise.
func HandleHealth(pool *pgxpool.Pool, rdb *redis.Client, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		resp := &HealthResponse{
			Status: "ok",
			DB:     "ok",
			Redis:  "ok",
		}

		if err := db.Ping(ctx, pool); err != nil {
			resp.DB = "error"
			resp.Status = "error"
			if log != nil {
				log.WithError(err).Warn("health: db ping failed")
			}
		}

		if err := cache.Ping(ctx, rdb); err != nil {
			resp.Redis = "error"
			resp.Status = "error"
			if log != nil {
				log.WithError(err).Warn("health: redis ping failed")
			}
		}

		httpStatus := http.StatusOK
		if resp.Status == "error" {
			httpStatus = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
