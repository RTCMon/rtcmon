// Package ratelimit implements a Redis sliding-window rate limiter keyed on
// AppID. Each request claims one slot in a sorted set; old entries outside the
// window are pruned atomically via a Lua script before the slot is granted.
package ratelimit

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/auth"
)

// rateLimitScript is a Redis Lua script that implements a sliding-window
// counter. It uses a sorted set where each entry has:
//   - score: Unix-second timestamp of the request
//   - member: a unique string (nanosecond timestamp) so concurrent requests
//     in the same second do not overwrite each other
//
// KEYS[1] = rate-limit key (e.g. "ratelimit:<appID>")
// ARGV[1] = max requests allowed in the window
// ARGV[2] = window size in seconds
// ARGV[3] = current Unix time in seconds (score)
// ARGV[4] = unique member string for this request
//
// Returns 1 if the request is allowed, 0 if the limit is exceeded.
const rateLimitScript = `
local key    = KEYS[1]
local limit  = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local now    = tonumber(ARGV[3])
local uid    = ARGV[4]

redis.call('ZREMRANGEBYSCORE', key, 0, now - window)
local count = redis.call('ZCARD', key)
if count >= limit then return 0 end
redis.call('ZADD', key, now, uid)
redis.call('EXPIRE', key, window)
return 1
`

// Config holds rate-limit parameters.
type Config struct {
	Max        int // maximum requests per window
	WindowSecs int // sliding window size in seconds
}

// RateLimiter executes the sliding-window Lua script against Redis.
// It is safe for concurrent use.
type RateLimiter struct {
	rdb     *redis.Client
	cfg     Config
	log     *logrus.Logger
	clockFn func() (nowSecs int64, uid string)
}

// New creates a RateLimiter with production defaults.
func New(rdb *redis.Client, cfg Config, log *logrus.Logger) *RateLimiter {
	return &RateLimiter{
		rdb:     rdb,
		cfg:     cfg,
		log:     log,
		clockFn: defaultClock,
	}
}

func defaultClock() (int64, string) {
	now := time.Now()
	return now.Unix(), strconv.FormatInt(now.UnixNano(), 10)
}

// Allow returns true when the request for appID is within the configured
// quota. It returns true (fail-open) on Redis errors so a Redis outage does
// not take down the ingest pipeline.
func (rl *RateLimiter) Allow(ctx context.Context, appID string) (bool, error) {
	nowSecs, uid := rl.clockFn()

	result, err := rl.rdb.Eval(ctx, rateLimitScript,
		[]string{"ratelimit:" + appID},
		rl.cfg.Max,
		rl.cfg.WindowSecs,
		nowSecs,
		uid,
	).Int()
	if err != nil {
		rl.log.WithError(err).Warn("ratelimit: Redis eval failed, allowing request")
		return true, err
	}
	return result == 1, nil
}

// Middleware returns a chi-compatible HTTP middleware that enforces the rate
// limit. It reads AppID from the JWT claims already placed in context by
// auth.Authenticate. Requests over the limit receive a 429 with
// Retry-After: 1.
func (rl *RateLimiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := auth.ClaimsFromContext(r.Context())
			if claims.AppID == "" {
				next.ServeHTTP(w, r)
				return
			}

			allowed, _ := rl.Allow(r.Context(), claims.AppID)
			if !allowed {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				body, _ := json.Marshal(map[string]string{"error": "rate limit exceeded"})
				_, _ = w.Write(body)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
