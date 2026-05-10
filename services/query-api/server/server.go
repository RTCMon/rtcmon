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

	"github.com/RTCMon/rtcmon/internal/session"
	"github.com/RTCMon/rtcmon/services/query-api/handler"
)

// Server is the query-api HTTP server.
type Server struct {
	router          *chi.Mux
	db              *pgxpool.Pool
	redis           *redis.Client
	log             *logrus.Logger
	sessions        *session.Store
	serverMasterKey []byte
}

// NewServer wires the chi router, middleware, and all routes.
func NewServer(
	_ context.Context,
	db *pgxpool.Pool,
	rdb *redis.Client,
	log *logrus.Logger,
	sessions *session.Store,
	serverMasterKey []byte,
) *Server {
	r := chi.NewRouter()

	s := &Server{
		router:          r,
		db:              db,
		redis:           rdb,
		log:             log,
		sessions:        sessions,
		serverMasterKey: serverMasterKey,
	}

	s.setupMiddleware()
	s.setupRoutes()
	return s
}

func (s *Server) setupMiddleware() {
	s.router.Use(middleware.RequestID)
	s.router.Use(loggingMiddleware(s.log))
	s.router.Use(csrfMiddleware())
	s.router.Use(middleware.Recoverer)
}

func (s *Server) setupRoutes() {
	// Public routes.
	s.router.Get("/health", handler.HandleHealth(s.db, s.redis, s.log))

	// Auth endpoints — session creation/destruction (no session required).
	s.router.Post("/auth/register", handler.HandleRegister(s.db, s.sessions, s.log))
	s.router.Post("/auth/login", handler.HandleLogin(s.db, s.sessions, s.log))
	s.router.Post("/auth/logout", handler.HandleLogout(s.sessions))

	// Session-required routes.
	s.router.Group(func(r chi.Router) {
		r.Use(s.sessionMiddleware())

		r.Get("/auth/me", handler.HandleMe())
		r.Patch("/auth/me", handler.HandlePatchMe(s.db, s.sessions, s.log))

		// Conference endpoints — BE-024/BE-025.
		r.Get("/v1/apps/{appId}/conferences", handler.HandleListConferences(s.db, s.log))
		r.Get("/v1/conferences/{conferenceId}", handler.HandleGetConference(s.db, s.log))

		// Connection stats time-series — BE-026.
		r.Get("/v1/connections/{connectionId}/stats", handler.HandleGetConnectionStats(s.db, s.redis, s.log))

		// Connection events — BE-027.
		r.Get("/v1/connections/{connectionId}/events", handler.HandleGetConnectionEvents(s.db, s.log))

		// User call history — BE-028.
		r.Get("/v1/users/{userId}/calls", handler.HandleGetUserCalls(s.db, s.log))

		// Analytics overview — BE-031.
		r.Get("/v1/apps/{appId}/analytics/overview", handler.HandleGetAnalyticsOverview(s.db, s.redis, s.log))

		// Analytics breakdown — BE-032.
		r.Get("/v1/apps/{appId}/analytics/breakdown", handler.HandleGetAnalyticsBreakdown(s.db, s.redis, s.log))

		// Server API key management — BE-021.
		r.Route("/v1/orgs/{orgId}/apps/{appId}", func(r chi.Router) {
			r.Get("/server-key", handler.HandleGetServerKey(s.db, s.log))
			r.Post("/server-key", handler.HandleGenerateServerKey(s.db, s.serverMasterKey, s.log))
			r.Delete("/server-key", handler.HandleDeleteServerKey(s.db, s.log))
			r.Post("/rotate-server-key", handler.HandleRotateServerKey(s.db, s.serverMasterKey, s.log))
		})
	})
}

// sessionMiddleware reads the "session" cookie, validates it against Redis, and
// stores the Data in the request context. Returns 401 when the cookie is absent
// or the session has expired.
func (s *Server) sessionMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie("session")
			if err != nil {
				writeUnauthorized(w)
				return
			}

			data, err := s.sessions.Get(r.Context(), cookie.Value)
			if err != nil {
				s.log.WithError(err).Warn("session middleware: store error")
				writeUnauthorized(w)
				return
			}
			if data == nil {
				writeUnauthorized(w)
				return
			}

			next.ServeHTTP(w, r.WithContext(session.WithContext(r.Context(), data)))
		})
	}
}

// csrfMiddleware rejects state-mutating requests whose Origin header is present
// but does not match the server's own scheme+host. Requests without an Origin
// header (programmatic API clients) pass through; SameSite=Strict cookies
// prevent cross-site cookie forwarding for browser requests.
func csrfMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
				if origin := r.Header.Get("Origin"); origin != "" {
					scheme := "http"
					if r.TLS != nil {
						scheme = "https"
					}
					if origin != scheme+"://"+r.Host {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte(`{"error":"invalid origin"}`))
						return
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}

func loggingMiddleware(log *logrus.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			requestID := middleware.GetReqID(r.Context())
			w.Header().Set("X-Request-ID", requestID)

			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			log.WithFields(logrus.Fields{
				"request_id":  requestID,
				"method":      r.Method,
				"path":        r.RequestURI,
				"status":      ww.Status(),
				"duration_ms": time.Since(start).Milliseconds(),
			}).Info("request completed")
		})
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func (s *Server) Listen(port int) error {
	s.log.WithField("port", port).Info("query-api listening")
	return http.ListenAndServe(fmt.Sprintf(":%d", port), s)
}
