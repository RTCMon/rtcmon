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

type HealthResponse struct {
	Status string `json:"status"`
	DB     string `json:"db"`
	Redis  string `json:"redis"`
}

func HandleHealth(pool *pgxpool.Pool, client *redis.Client, log *logrus.Logger) http.HandlerFunc {
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
			log.WithError(err).Warn("health check: db ping failed")
		}

		if err := cache.Ping(ctx, client); err != nil {
			resp.Redis = "error"
			resp.Status = "error"
			log.WithError(err).Warn("health check: redis ping failed")
		}

		status := http.StatusOK
		if resp.Status == "error" {
			status = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(resp)
	}
}
