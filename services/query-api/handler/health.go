package handler

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/cache"
	"github.com/RTCMon/rtcmon/internal/db"
)

func HandleHealth(pool *pgxpool.Pool, rdb *redis.Client, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbStatus := "ok"
		if err := db.Ping(r.Context(), pool); err != nil {
			log.WithError(err).Warn("health: db ping failed")
			dbStatus = "error"
		}

		redisStatus := "ok"
		if err := cache.Ping(r.Context(), rdb); err != nil {
			log.WithError(err).Warn("health: redis ping failed")
			redisStatus = "error"
		}

		status := http.StatusOK
		overall := "ok"
		if dbStatus != "ok" || redisStatus != "ok" {
			status = http.StatusServiceUnavailable
			overall = "degraded"
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": overall,
			"db":     dbStatus,
			"redis":  redisStatus,
		})
	}
}
