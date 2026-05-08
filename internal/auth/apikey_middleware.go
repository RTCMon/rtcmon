package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
)

// serverClaimsKey is an unexported context key for ServerClaims. Uses a
// distinct type from contextKey{} to prevent collisions with JWT Claims.
type serverClaimsKey struct{}

// ServerClaims holds the app identity resolved by the HMAC middleware from the
// DB. Stored in the request context after successful authentication.
type ServerClaims struct {
	AppID  int64
	OrgID  int64
	APIKey string // hex key string, used for rate-limit scoping
}

// ServerClaimsFromContext retrieves the *ServerClaims placed by
// AuthenticateAPIKey. Returns nil (not a zero-value struct) when absent, so
// callers can distinguish "no auth ran" from "auth ran but claims are empty".
func ServerClaimsFromContext(ctx context.Context) *ServerClaims {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(serverClaimsKey{}).(*ServerClaims)
	return c
}

// WithServerClaims returns a copy of r with sc stored as ServerClaims in the
// context. Mirrors what AuthenticateAPIKey does internally; useful in test
// suites and synthetic middleware chains that need to bypass real HMAC auth.
func WithServerClaims(r *http.Request, sc *ServerClaims) *http.Request {
	ctx := context.WithValue(r.Context(), serverClaimsKey{}, sc)
	return r.WithContext(ctx)
}

// apiKeyMiddleware holds the dependencies for the HMAC auth flow. The
// lookupFn and nonceFn fields are injectable for unit testing — production
// code uses the defaultLookup / defaultNonce methods wired to real DB/Redis.
type apiKeyMiddleware struct {
	masterKey []byte
	log       *logrus.Logger

	// lookupFn resolves an API key to its app identity and encrypted secret.
	// Returns errKeyNotFound when the key does not exist.
	lookupFn func(ctx context.Context, apiKey string) (appID, orgID int64, secretEnc string, err error)

	// nonceFn atomically sets a nonce key with the given TTL. Returns true when
	// the key was newly created (request is unique), false when it already
	// existed (replay detected). On Redis error: callers treat it as fail-open.
	nonceFn func(ctx context.Context, nonceKey string, ttl time.Duration) (set bool, err error)

	// nowFn returns the current time. Overridable in tests.
	nowFn func() time.Time
}

var errKeyNotFound = errors.New("apikey: server_api_key not found")

// AuthenticateAPIKey returns a chi-compatible middleware that enforces HMAC
// request authentication as described in RFC §3.11.4.
//
// masterKey must be exactly 32 bytes (validated by config.Load when
// SERVER_MASTER_KEY is set). Callers should not pass a nil or short masterKey.
func AuthenticateAPIKey(db *pgxpool.Pool, rdb *redis.Client, masterKey []byte, log *logrus.Logger) func(http.Handler) http.Handler {
	m := &apiKeyMiddleware{
		masterKey: masterKey,
		log:       log,
		nowFn:     time.Now,
	}
	m.lookupFn = func(ctx context.Context, apiKey string) (int64, int64, string, error) {
		return m.defaultLookup(ctx, db, apiKey)
	}
	m.nonceFn = func(ctx context.Context, nonceKey string, ttl time.Duration) (bool, error) {
		return m.defaultNonce(ctx, rdb, nonceKey, ttl)
	}
	return m.handler()
}

func (m *apiKeyMiddleware) handler() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Step 1: Extract required headers.
			apiKey := r.Header.Get("X-API-Key")
			tsStr := r.Header.Get("X-Timestamp")
			sigHex := r.Header.Get("X-Signature")
			if apiKey == "" || tsStr == "" || sigHex == "" {
				writeAPIKeyError(w, http.StatusUnauthorized, "missing authentication headers")
				return
			}

			// Step 2: Timestamp window check — fail fast before any DB I/O.
			ts, err := strconv.ParseInt(tsStr, 10, 64)
			if err != nil {
				writeAPIKeyError(w, http.StatusUnauthorized, "timestamp out of window")
				return
			}
			diff := m.nowFn().Unix() - ts
			if diff < -300 || diff > 300 {
				writeAPIKeyError(w, http.StatusUnauthorized, "timestamp out of window")
				return
			}

			// Step 3: Resolve key → app identity + encrypted secret.
			appID, orgID, secretEnc, err := m.lookupFn(r.Context(), apiKey)
			if err != nil {
				writeAPIKeyError(w, http.StatusUnauthorized, "invalid credentials")
				return
			}

			// Step 4: Decrypt the stored secret.
			secretBytes, err := DecryptSecret(m.masterKey, secretEnc)
			if err != nil {
				writeAPIKeyError(w, http.StatusInternalServerError, "internal error")
				return
			}

			// Step 5: Read body, verify HMAC, then restore r.Body for the handler.
			body, err := io.ReadAll(r.Body)
			if err != nil {
				writeAPIKeyError(w, http.StatusBadRequest, "cannot read request body")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			if !VerifyHMAC(secretBytes, r.Method, r.URL.Path, ts, body, sigHex) {
				writeAPIKeyError(w, http.StatusUnauthorized, "invalid credentials")
				return
			}

			// Step 6: Replay protection via Redis nonce.
			sigPrefix := sigHex
			if len(sigPrefix) > 16 {
				sigPrefix = sigPrefix[:16]
			}
			nonceKey := fmt.Sprintf("server_nonce:%s:%d:%s", apiKey, ts, sigPrefix)
			set, nonceErr := m.nonceFn(r.Context(), nonceKey, 600*time.Second)
			if nonceErr == nil && !set {
				writeAPIKeyError(w, http.StatusUnauthorized, "duplicate request")
				return
			}
			if nonceErr != nil {
				m.log.WithError(nonceErr).Warn("apikey: Redis nonce check failed, replay protection disabled")
			}

			// Step 7: Stamp claims and call next handler.
			claims := &ServerClaims{AppID: appID, OrgID: orgID, APIKey: apiKey}
			ctx := context.WithValue(r.Context(), serverClaimsKey{}, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// defaultLookup performs the real DB query for the HMAC middleware.
func (m *apiKeyMiddleware) defaultLookup(ctx context.Context, db *pgxpool.Pool, apiKey string) (int64, int64, string, error) {
	var appID, orgID int64
	var secretEnc string
	err := db.QueryRow(ctx,
		`SELECT id, org_id, server_api_secret_enc FROM apps WHERE server_api_key = $1`,
		apiKey,
	).Scan(&appID, &orgID, &secretEnc)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, "", errKeyNotFound
	}
	if err != nil {
		return 0, 0, "", fmt.Errorf("apikey: db lookup: %w", err)
	}
	return appID, orgID, secretEnc, nil
}

// defaultNonce performs the real Redis SetNX for replay protection.
func (m *apiKeyMiddleware) defaultNonce(ctx context.Context, rdb *redis.Client, nonceKey string, ttl time.Duration) (bool, error) {
	return rdb.SetNX(ctx, nonceKey, 1, ttl).Result()
}

// writeAPIKeyError writes a JSON error response with the given status code.
func writeAPIKeyError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(body)
}
