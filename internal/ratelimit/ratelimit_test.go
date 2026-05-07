package ratelimit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/auth"
)

// fakeClock provides deterministic time for tests. nowSecs is the current
// Unix-second value; counter increments on every call so each request gets
// a unique UID even within the same fake second.
type fakeClock struct {
	mu      sync.Mutex
	nowSecs int64
	counter int64
}

func (fc *fakeClock) tick() (int64, string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.counter++
	return fc.nowSecs, fmt.Sprintf("%d", fc.counter)
}

func (fc *fakeClock) advance(secs int64) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.nowSecs += secs
}

// newTestLimiter starts a miniredis server and returns a RateLimiter wired to
// it with the given config and a fakeClock (so time is under test control).
func newTestLimiter(t *testing.T, cfg Config) (*RateLimiter, *fakeClock) {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	log := logrus.New()
	log.SetOutput(io.Discard)

	fc := &fakeClock{nowSecs: 1_000_000} // arbitrary fixed start
	rl := &RateLimiter{
		rdb:     rdb,
		cfg:     cfg,
		log:     log,
		clockFn: fc.tick,
	}
	return rl, fc
}

// signedToken generates a valid HS256 JWT with the given appID for use in
// middleware integration tests.
func signedToken(t *testing.T, secret, appID string) string {
	t.Helper()
	claims := auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		AppID:        appID,
		ConferenceID: "conf-1",
		UserID:       "user-1",
	}

	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}

func cfg100per60() Config { return Config{Max: 100, WindowSecs: 60} }

// TestRateLimit_UnderLimit — 99 requests all succeed.
func TestRateLimit_UnderLimit(t *testing.T) {
	rl, _ := newTestLimiter(t, cfg100per60())
	ctx := context.Background()

	for i := range 99 {
		allowed, err := rl.Allow(ctx, "app-a")
		if err != nil {
			t.Fatalf("request %d: unexpected error: %v", i+1, err)
		}
		if !allowed {
			t.Fatalf("request %d: want allowed, got denied", i+1)
		}
	}
}

// TestRateLimit_AtLimit — 100th request succeeds, 101st returns false.
func TestRateLimit_AtLimit(t *testing.T) {
	rl, _ := newTestLimiter(t, cfg100per60())
	ctx := context.Background()

	for i := range 100 {
		allowed, err := rl.Allow(ctx, "app-a")
		if err != nil {
			t.Fatalf("request %d: unexpected error: %v", i+1, err)
		}
		if !allowed {
			t.Fatalf("request %d (should succeed): got denied", i+1)
		}
	}

	// 101st must be denied.
	allowed, err := rl.Allow(ctx, "app-a")
	if err != nil {
		t.Fatalf("101st request: unexpected error: %v", err)
	}
	if allowed {
		t.Error("101st request: want denied, got allowed")
	}
}

// TestRateLimit_WindowReset — after advancing the clock past the window the
// counter resets and requests succeed again.
func TestRateLimit_WindowReset(t *testing.T) {
	rl, fc := newTestLimiter(t, cfg100per60())
	ctx := context.Background()

	// Exhaust the quota.
	for range 100 {
		rl.Allow(ctx, "app-a") //nolint:errcheck
	}
	// Confirm 101st is denied.
	if allowed, _ := rl.Allow(ctx, "app-a"); allowed {
		t.Fatal("expected denied before window reset")
	}

	// Advance the fake clock past the window (61 seconds > 60-second window).
	fc.advance(61)

	// Should succeed again.
	allowed, err := rl.Allow(ctx, "app-a")
	if err != nil {
		t.Fatalf("post-reset: unexpected error: %v", err)
	}
	if !allowed {
		t.Error("post-reset: want allowed, got denied")
	}
}

// TestRateLimit_ScopedPerApp — exhausting app-A's quota does not affect app-B.
func TestRateLimit_ScopedPerApp(t *testing.T) {
	rl, _ := newTestLimiter(t, cfg100per60())
	ctx := context.Background()

	// Exhaust app-A (including the over-limit call).
	for range 101 {
		rl.Allow(ctx, "app-a") //nolint:errcheck
	}

	// app-B should still be allowed.
	allowed, err := rl.Allow(ctx, "app-b")
	if err != nil {
		t.Fatalf("app-b: unexpected error: %v", err)
	}
	if !allowed {
		t.Error("app-b: want allowed, got denied (quota should be independent)")
	}
}

// TestRateLimit_RetryAfterHeader — the middleware returns 429 with the
// Retry-After: 1 header when the quota is exhausted. Claims are injected via
// a real auth.Authenticate call so the context key matches production.
func TestRateLimit_RetryAfterHeader(t *testing.T) {
	const secret = "test-secret"
	rl, _ := newTestLimiter(t, Config{Max: 1, WindowSecs: 60})

	// Chain: auth.Authenticate → rl.Middleware → no-op handler
	handler := auth.Authenticate(secret)(
		rl.Middleware()(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		),
	)

	doRequest := func(appID string) *httptest.ResponseRecorder {
		tok := signedToken(t, secret, appID)
		req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	// First request consumes the single allowed slot.
	if w := doRequest("app-a"); w.Code != http.StatusOK {
		t.Fatalf("1st request: want 200, got %d", w.Code)
	}

	// Second request must be rate-limited.
	w := doRequest("app-a")
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("want 429, got %d", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra != "1" {
		t.Errorf("want Retry-After: 1, got %q", ra)
	}
}
