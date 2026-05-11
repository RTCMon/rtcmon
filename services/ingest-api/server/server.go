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
	httpSwagger "github.com/swaggo/http-swagger/v2"

	"github.com/RTCMon/rtcmon/internal/auth"
	"github.com/RTCMon/rtcmon/internal/metrics"
	"github.com/RTCMon/rtcmon/internal/ratelimit"
	_ "github.com/RTCMon/rtcmon/services/ingest-api/docs"
	"github.com/RTCMon/rtcmon/services/ingest-api/handler"
)

type Server struct {
	router           *chi.Mux
	db               *pgxpool.Pool
	redis            *redis.Client
	log              *logrus.Logger
	jwtSecret        string
	rateLimiter      *ratelimit.RateLimiter
	serverMasterKey  []byte
	serverRateLimiter *ratelimit.RateLimiter
	enqueue          handler.EnqueueFn
	emosTrigger      handler.EMOSTriggerFn
}

func NewServer(
	ctx context.Context,
	db *pgxpool.Pool,
	rdb *redis.Client,
	log *logrus.Logger,
	jwtSecret string,
	rl *ratelimit.RateLimiter,
	serverMasterKey []byte,
	serverRL *ratelimit.RateLimiter,
	enqueue handler.EnqueueFn,
	emosTrigger handler.EMOSTriggerFn,
) *Server {
	r := chi.NewRouter()

	s := &Server{
		router:            r,
		db:                db,
		redis:             rdb,
		log:               log,
		jwtSecret:         jwtSecret,
		rateLimiter:       rl,
		serverMasterKey:   serverMasterKey,
		serverRateLimiter: serverRL,
		enqueue:           enqueue,
		emosTrigger:       emosTrigger,
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
	// Public routes — no auth required.
	s.router.Get("/health", handler.HandleHealth(s.db, s.redis, s.log))
	s.router.Get("/swagger/*", httpSwagger.Handler())

	// JWT-authenticated routes.
	s.router.Group(func(r chi.Router) {
		r.Use(auth.Authenticate(s.jwtSecret))
		if s.rateLimiter != nil {
			r.Use(s.rateLimiter.Middleware())
		}
		r.Post("/v1/events", handler.HandleEvents(s.log, s.enqueue))
		r.Post("/v1/conferences/{conferenceID}/end",
			handler.HandleEndConference(s.db, s.redis, s.log, s.emosTrigger))
	})

	// HMAC-authenticated server SDK routes.
	// Only registered when SERVER_MASTER_KEY is configured; a missing key would
	// make decryption impossible and silently accept invalid requests.
	if len(s.serverMasterKey) == 32 {
		s.router.Group(func(r chi.Router) {
			r.Use(auth.AuthenticateAPIKey(s.db, s.redis, s.serverMasterKey, s.log))
			if s.serverRateLimiter != nil {
				r.Use(s.serverRateLimiter.ServerMiddleware())
			}
			r.Post("/v1/server/events", handler.HandleServerEvents(s.log, s.enqueue))
			r.Post("/v1/server/conferences/{conferenceID}/end",
				handler.HandleEndConference(s.db, s.redis, s.log, s.emosTrigger))
		})
	} else {
		s.log.Warn("SERVER_MASTER_KEY not configured — /v1/server/* endpoints disabled")
	}
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
			status := ww.Status()

			var statusClass string
			switch {
			case status >= 500:
				statusClass = "5xx"
			case status >= 400:
				statusClass = "4xx"
			default:
				statusClass = "2xx"
			}
			metrics.IngestRequestsTotal.WithLabelValues(statusClass).Inc()

			log.WithFields(logrus.Fields{
				"request_id":  requestID,
				"method":      r.Method,
				"path":        r.RequestURI,
				"status":      status,
				"duration_ms": duration,
			}).Info("request completed")
		})
	}
}

func (s *Server) Listen(port int) error {
	s.log.WithField("port", port).Info("ingest-api listening")
	return http.ListenAndServe(fmt.Sprintf(":%d", port), s)
}
