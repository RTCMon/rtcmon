package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// ─── unit tests (no external dependencies) ───────────────────────────────────

func TestHealthResponse_ValidJSON(t *testing.T) {
	resp := &HealthResponse{Status: "ok", DB: "ok", Redis: "ok"}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded HealthResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded != *resp {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, *resp)
	}
}

func TestHealthResponse_AllHealthy(t *testing.T) {
	resp := HealthResponse{Status: "ok", DB: "ok", Redis: "ok"}
	if resp.Status != "ok" || resp.DB != "ok" || resp.Redis != "ok" {
		t.Errorf("unexpected values: %+v", resp)
	}
}

func TestHealthResponse_DBDown(t *testing.T) {
	resp := HealthResponse{Status: "error", DB: "error", Redis: "ok"}
	if resp.Status != "error" || resp.DB != "error" {
		t.Errorf("unexpected values: %+v", resp)
	}
	if resp.Redis != "ok" {
		t.Errorf("redis should be ok, got %q", resp.Redis)
	}
}

func TestHealthResponse_RedisDown(t *testing.T) {
	resp := HealthResponse{Status: "error", DB: "ok", Redis: "error"}
	if resp.Status != "error" || resp.Redis != "error" {
		t.Errorf("unexpected values: %+v", resp)
	}
	if resp.DB != "ok" {
		t.Errorf("db should be ok, got %q", resp.DB)
	}
}

func TestHealthResponse_ContentType(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w.Body).Encode(HealthResponse{Status: "ok", DB: "ok", Redis: "ok"})

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
}

// ─── integration tests (skip when TEST_DB_URL / TEST_REDIS_URL not set) ──────

func testPoolOrSkip(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("TEST_DB_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testRedisOrSkip(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; skipping integration test")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("redis.ParseURL: %v", err)
	}
	client := redis.NewClient(opt)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestHealth_AllHealthy(t *testing.T) {
	pool := testPoolOrSkip(t)
	rdb := testRedisOrSkip(t)

	h := HandleHealth(pool, rdb, nil)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	var resp HealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "ok" || resp.DB != "ok" || resp.Redis != "ok" {
		t.Errorf("response: got %+v, want all ok", resp)
	}
}

func TestHealth_DBDown(t *testing.T) {
	rdb := testRedisOrSkip(t)

	// Point DB at an unreachable host.
	pool, err := pgxpool.New(context.Background(),
		"postgres://user:pass@localhost:1/nonexistent?connect_timeout=1")
	if err == nil {
		// Pool creation may succeed (lazy connect); only the ping will fail.
		t.Cleanup(pool.Close)
	}

	h := HandleHealth(pool, rdb, nil)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", w.Code)
	}

	var resp HealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DB != "error" {
		t.Errorf("db: got %q, want error", resp.DB)
	}
}

func TestHealth_RedisDown(t *testing.T) {
	pool := testPoolOrSkip(t)

	// Point Redis at an unreachable address.
	rdb := redis.NewClient(&redis.Options{
		Addr:        fmt.Sprintf("localhost:%d", 19999),
		DialTimeout: 500,
	})
	t.Cleanup(func() { _ = rdb.Close() })

	h := HandleHealth(pool, rdb, nil)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", w.Code)
	}

	var resp HealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Redis != "error" {
		t.Errorf("redis: got %q, want error", resp.Redis)
	}
}
