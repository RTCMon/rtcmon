package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/services/ingest-api/handler"
)

type Server struct {
	router *chi.Mux
	db     *pgxpool.Pool
	redis  *redis.Client
	log    *logrus.Logger
}

func NewServer(ctx context.Context, db *pgxpool.Pool, redis *redis.Client, log *logrus.Logger) *Server {
	r := chi.NewRouter()

	s := &Server{
		router: r,
		db:     db,
		redis:  redis,
		log:    log,
	}

	s.setupMiddleware()
	s.setupRoutes()

	return s
}

func (s *Server) setupMiddleware() {
	s.router.Use(middleware.RequestID)
	s.router.Use(loggingMiddleware(s.log))
	s.router.Use(middleware.Recoverer)
}

func (s *Server) setupRoutes() {
	s.router.Get("/health", handler.HandleHealth(s.db, s.redis, s.log))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func loggingMiddleware(log *logrus.Logger) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			requestID := middleware.GetReqID(r.Context())

			w.Header().Set("X-Request-ID", requestID)

			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			duration := time.Since(start).Milliseconds()

			log.WithFields(logrus.Fields{
				"request_id":  requestID,
				"method":      r.Method,
				"path":        r.RequestURI,
				"status":      ww.Status(),
				"duration_ms": duration,
			}).Info("request completed")
		})
	}
}

func (s *Server) Listen(port int) error {
	s.log.WithField("port", port).Info("ingest-api listening")
	return http.ListenAndServe(fmt.Sprintf(":%d", port), s)
}
