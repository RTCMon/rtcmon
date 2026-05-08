// Tests for the HMAC API key middleware. Uses the unexported apiKeyMiddleware
// struct directly so that DB and Redis dependencies can be stubbed without a
// running Postgres or a full Redis instance (miniredis covers nonce tests).
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
)

// testMasterKey32 is a 32-byte key for encryption in tests.
var testMasterKey32 = []byte("abcdefghijklmnopqrstuvwxyz012345") // 32 bytes

// encryptedTestSecret encrypts the given secret bytes with testMasterKey32
// for use as a stubbed DB return value.
func encryptedTestSecret(t *testing.T, secretHex string) string {
	t.Helper()
	secretBytes, err := hex.DecodeString(secretHex)
	if err != nil {
		t.Fatalf("decode test secret hex: %v", err)
	}
	enc, err := EncryptSecret(testMasterKey32, secretBytes)
	if err != nil {
		t.Fatalf("encrypt test secret: %v", err)
	}
	return enc
}

// buildTestSignature computes the HMAC-SHA256 signature for a test request.
func buildTestSignature(secretHex, method, path string, ts int64, body []byte) string {
	secretBytes, _ := hex.DecodeString(secretHex)
	canonical := BuildCanonical(method, path, ts, body)
	mac := hmac.New(sha256.New, secretBytes)
	mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

// knownSecretHex is a 64-char hex secret used across tests.
const knownSecretHex = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// newStubMiddleware builds an apiKeyMiddleware with controllable dependencies.
// lookupOK controls whether the DB lookup succeeds or returns errKeyNotFound.
// rdb is used for the real nonce check; pass nil to get a miniredis instance.
func newStubMiddleware(t *testing.T, lookupOK bool, rdb *redis.Client) (*apiKeyMiddleware, *miniredis.Miniredis) {
	t.Helper()
	var mr *miniredis.Miniredis
	if rdb == nil {
		mr = miniredis.RunT(t)
		rdb = redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
	}

	enc := encryptedTestSecret(t, knownSecretHex)

	m := &apiKeyMiddleware{
		masterKey: testMasterKey32,
		log:       logrus.New(),
		nowFn:     time.Now,
		lookupFn: func(_ context.Context, apiKey string) (int64, int64, string, error) {
			if !lookupOK {
				return 0, 0, "", errKeyNotFound
			}
			return 42, 7, enc, nil
		},
		nonceFn: func(ctx context.Context, nonceKey string, ttl time.Duration) (bool, error) {
			return rdb.SetNX(ctx, nonceKey, 1, ttl).Result()
		},
	}
	return m, mr
}

// doRequest fires a test request through the middleware. If body is nil an
// empty body is used. The returned ResponseRecorder contains the response.
func doRequest(t *testing.T, h http.Handler, method, path string, headers map[string]string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader io.Reader = strings.NewReader("")
	if body != nil {
		bodyReader = strings.NewReader(string(body))
	}
	req := httptest.NewRequest(method, path, bodyReader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// noopHandler is a simple handler that records whether it was called.
func noopHandler(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		w.WriteHeader(http.StatusAccepted)
	})
}

// ----- Step 1: Missing headers -----------------------------------------------

func TestHMACMiddleware_MissingAPIKey(t *testing.T) {
	m, _ := newStubMiddleware(t, false, nil)
	handler := m.handler()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	w := doRequest(t, handler, "POST", "/v1/server/events", map[string]string{
		"X-Timestamp": "1700000000",
		"X-Signature": "abc",
	}, []byte(`{}`))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "missing authentication headers") {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

func TestHMACMiddleware_MissingTimestamp(t *testing.T) {
	m, _ := newStubMiddleware(t, false, nil)
	handler := m.handler()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	w := doRequest(t, handler, "POST", "/v1/server/events", map[string]string{
		"X-API-Key":   "somekey",
		"X-Signature": "abc",
	}, []byte(`{}`))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

func TestHMACMiddleware_MissingSignature(t *testing.T) {
	m, _ := newStubMiddleware(t, false, nil)
	handler := m.handler()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	w := doRequest(t, handler, "POST", "/v1/server/events", map[string]string{
		"X-API-Key":   "somekey",
		"X-Timestamp": "1700000000",
	}, []byte(`{}`))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

// ----- Step 2: Timestamp window ----------------------------------------------

func TestHMACMiddleware_StaleTimestamp_Past(t *testing.T) {
	m, _ := newStubMiddleware(t, false, nil)
	// Override nowFn so the middleware thinks it's 1700000000.
	m.nowFn = func() time.Time { return time.Unix(1700000000, 0) }
	handler := m.handler()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	// ts = now - 301 → outside window
	staleTS := fmt.Sprintf("%d", int64(1700000000)-301)
	w := doRequest(t, handler, "POST", "/v1/server/events", map[string]string{
		"X-API-Key":   "somekey",
		"X-Timestamp": staleTS,
		"X-Signature": "abc",
	}, nil)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "timestamp out of window") {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

func TestHMACMiddleware_StaleTimestamp_Future(t *testing.T) {
	m, _ := newStubMiddleware(t, false, nil)
	m.nowFn = func() time.Time { return time.Unix(1700000000, 0) }
	handler := m.handler()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	// ts = now + 301 → outside window
	futureTS := fmt.Sprintf("%d", int64(1700000000)+301)
	w := doRequest(t, handler, "POST", "/v1/server/events", map[string]string{
		"X-API-Key":   "somekey",
		"X-Timestamp": futureTS,
		"X-Signature": "abc",
	}, nil)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

// ----- Step 3: Unknown key ---------------------------------------------------

func TestHMACMiddleware_UnknownKey(t *testing.T) {
	m, _ := newStubMiddleware(t, false /* lookupOK=false */, nil)
	m.nowFn = func() time.Time { return time.Unix(1700000000, 0) }
	handler := m.handler()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	ts := int64(1700000000)
	body := []byte(`{}`)
	sig := buildTestSignature(knownSecretHex, "POST", "/v1/server/events", ts, body)

	w := doRequest(t, handler, "POST", "/v1/server/events", map[string]string{
		"X-API-Key":   "unknown-key",
		"X-Timestamp": fmt.Sprintf("%d", ts),
		"X-Signature": sig,
	}, body)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid credentials") {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

// ----- Step 5: Wrong signature -----------------------------------------------

func TestHMACMiddleware_WrongSignature(t *testing.T) {
	m, _ := newStubMiddleware(t, true, nil)
	m.nowFn = func() time.Time { return time.Unix(1700000000, 0) }
	handler := m.handler()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	ts := int64(1700000000)
	body := []byte(`{}`)
	// Build signature with a different secret.
	wrongSig := buildTestSignature("aaaa"+knownSecretHex[:60], "POST", "/v1/server/events", ts, body)

	w := doRequest(t, handler, "POST", "/v1/server/events", map[string]string{
		"X-API-Key":   "valid-key",
		"X-Timestamp": fmt.Sprintf("%d", ts),
		"X-Signature": wrongSig,
	}, body)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

// ----- Steps 6–7: Valid request + nonce + claims -----------------------------

func TestHMACMiddleware_Valid(t *testing.T) {
	m, _ := newStubMiddleware(t, true, nil)
	fixedNow := int64(1700000000)
	m.nowFn = func() time.Time { return time.Unix(fixedNow, 0) }

	var nextCalled bool
	handler := m.handler()(noopHandler(&nextCalled))

	ts := fixedNow
	body := []byte(`{"conference_id":"c1","session_id":"s1","connection_id":"x1","events":[{"ts":1}]}`)
	sig := buildTestSignature(knownSecretHex, "POST", "/v1/server/events", ts, body)

	w := doRequest(t, handler, "POST", "/v1/server/events", map[string]string{
		"X-API-Key":   "valid-key",
		"X-Timestamp": fmt.Sprintf("%d", ts),
		"X-Signature": sig,
	}, body)

	if w.Code != http.StatusAccepted {
		t.Errorf("want 202, got %d; body: %s", w.Code, w.Body.String())
	}
	if !nextCalled {
		t.Error("expected next handler to be called")
	}
}

func TestHMACMiddleware_AppIDInContext(t *testing.T) {
	m, _ := newStubMiddleware(t, true, nil)
	fixedNow := int64(1700000000)
	m.nowFn = func() time.Time { return time.Unix(fixedNow, 0) }

	var gotClaims *ServerClaims
	handler := m.handler()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClaims = ServerClaimsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	ts := fixedNow
	body := []byte(`{}`)
	sig := buildTestSignature(knownSecretHex, "POST", "/v1/server/events", ts, body)

	doRequest(t, handler, "POST", "/v1/server/events", map[string]string{
		"X-API-Key":   "valid-key",
		"X-Timestamp": fmt.Sprintf("%d", ts),
		"X-Signature": sig,
	}, body)

	if gotClaims == nil {
		t.Fatal("ServerClaims not in context")
	}
	// newStubMiddleware returns appID=42, orgID=7.
	if gotClaims.AppID != 42 {
		t.Errorf("AppID = %d, want 42", gotClaims.AppID)
	}
	if gotClaims.OrgID != 7 {
		t.Errorf("OrgID = %d, want 7", gotClaims.OrgID)
	}
}

func TestHMACMiddleware_ReplayProtection(t *testing.T) {
	m, _ := newStubMiddleware(t, true, nil)
	fixedNow := int64(1700000000)
	m.nowFn = func() time.Time { return time.Unix(fixedNow, 0) }

	var nextCalls int
	handler := m.handler()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusAccepted)
	}))

	ts := fixedNow
	body := []byte(`{}`)
	sig := buildTestSignature(knownSecretHex, "POST", "/v1/server/events", ts, body)
	headers := map[string]string{
		"X-API-Key":   "valid-key",
		"X-Timestamp": fmt.Sprintf("%d", ts),
		"X-Signature": sig,
	}

	// First request must succeed.
	w1 := doRequest(t, handler, "POST", "/v1/server/events", headers, body)
	if w1.Code != http.StatusAccepted {
		t.Fatalf("1st request: want 202, got %d", w1.Code)
	}

	// Second identical request must be rejected as duplicate.
	w2 := doRequest(t, handler, "POST", "/v1/server/events", headers, body)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("2nd request (replay): want 401, got %d", w2.Code)
	}
	if !strings.Contains(w2.Body.String(), "duplicate request") {
		t.Errorf("unexpected body: %s", w2.Body.String())
	}
	if nextCalls != 1 {
		t.Errorf("next handler called %d times, want 1", nextCalls)
	}
}

func TestHMACMiddleware_ReplayExpired(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	m, _ := newStubMiddleware(t, true, rdb)
	fixedNow := int64(1700000000)
	m.nowFn = func() time.Time { return time.Unix(fixedNow, 0) }

	handler := m.handler()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))

	ts := fixedNow
	body := []byte(`{}`)
	sig := buildTestSignature(knownSecretHex, "POST", "/v1/server/events", ts, body)
	headers := map[string]string{
		"X-API-Key":   "valid-key",
		"X-Timestamp": fmt.Sprintf("%d", ts),
		"X-Signature": sig,
	}

	// First request succeeds.
	if w := doRequest(t, handler, "POST", "/v1/server/events", headers, body); w.Code != http.StatusAccepted {
		t.Fatalf("1st request: want 202, got %d", w.Code)
	}

	// Second (immediate replay) must fail.
	if w := doRequest(t, handler, "POST", "/v1/server/events", headers, body); w.Code != http.StatusUnauthorized {
		t.Fatalf("2nd request (before TTL): want 401, got %d", w.Code)
	}

	// Fast-forward miniredis clock past the 600s TTL.
	mr.FastForward(601 * time.Second)

	// After TTL expiry the same request must succeed again.
	// Also advance the middleware clock so the timestamp is still within window.
	m.nowFn = func() time.Time { return time.Unix(fixedNow+601, 0) }
	headers["X-Timestamp"] = fmt.Sprintf("%d", fixedNow+601)
	newSig := buildTestSignature(knownSecretHex, "POST", "/v1/server/events", fixedNow+601, body)
	headers["X-Signature"] = newSig

	w3 := doRequest(t, handler, "POST", "/v1/server/events", headers, body)
	if w3.Code != http.StatusAccepted {
		t.Errorf("3rd request (after TTL): want 202, got %d; body: %s", w3.Code, w3.Body.String())
	}
}

// TestServerClaimsFromContext_Nil verifies nil context returns nil without panic.
func TestServerClaimsFromContext_Nil(t *testing.T) {
	if got := ServerClaimsFromContext(nil); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

// TestServerClaimsFromContext_Empty verifies missing claims returns nil.
func TestServerClaimsFromContext_Empty(t *testing.T) {
	ctx := context.Background()
	if got := ServerClaimsFromContext(ctx); got != nil {
		t.Errorf("expected nil for empty context, got %+v", got)
	}
}

// ----- Helpers ---------------------------------------------------------------

// stubLookupError tests that a generic DB error (not ErrNoRows) also results
// in 401 rather than 500 — callers must not leak internal errors.
func TestHMACMiddleware_DBError_Returns401(t *testing.T) {
	m, _ := newStubMiddleware(t, false, nil)
	m.lookupFn = func(_ context.Context, _ string) (int64, int64, string, error) {
		return 0, 0, "", errors.New("db: connection lost")
	}
	m.nowFn = func() time.Time { return time.Unix(1700000000, 0) }

	handler := m.handler()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ts := int64(1700000000)
	body := []byte(`{}`)
	sig := buildTestSignature(knownSecretHex, "POST", "/v1/server/events", ts, body)

	w := doRequest(t, handler, "POST", "/v1/server/events", map[string]string{
		"X-API-Key":   "some-key",
		"X-Timestamp": fmt.Sprintf("%d", ts),
		"X-Signature": sig,
	}, body)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401 on DB error, got %d", w.Code)
	}
}
