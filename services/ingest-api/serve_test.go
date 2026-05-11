package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/emos"
	"github.com/RTCMon/rtcmon/internal/ingest"
	"github.com/RTCMon/rtcmon/internal/model"
	"github.com/RTCMon/rtcmon/internal/retention"
	"github.com/RTCMon/rtcmon/internal/stale"
	"github.com/RTCMon/rtcmon/internal/worker"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func shutdownLog() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

// freePort returns an unused TCP port on localhost.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// noopPool is a minimal worker.Pool with no-op Shutdown (no real goroutines).
// For tests that don't exercise the worker path.
func noopWorkerPool() *worker.Pool {
	return worker.New(
		worker.Config{WorkerCount: 1, BatchSize: 1, FlushIntervalMs: 100, ChannelCap: 1},
		func(_ context.Context, _ []model.IngestPayload) {},
		shutdownLog(),
	)
}

func noopEmosCh() chan int64 {
	return make(chan int64, 1)
}

// noopStaleJob creates a stale.Job that never ticks (24h interval) and has no
// DB — safe to use in tests that only exercise the HTTP/worker shutdown path.
func noopStaleJob() *stale.Job {
	j := stale.New(nil, nil, 24*time.Hour, 15*time.Minute, emos.DefaultLossCoeff, nil)
	j.Start()
	return j
}

// noopRetentionJob creates a retention.Job that fires only at 3 AM tomorrow and
// has no DB — safe to use in tests that only exercise the HTTP/worker shutdown path.
func noopRetentionJob() *retention.Job {
	j := retention.New(nil, nil, "0 3 * * *")
	j.Start()
	return j
}

func sendSignalAfter(sigCh chan<- os.Signal, d time.Duration) {
	time.AfterFunc(d, func() { sigCh <- syscall.SIGTERM })
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestGracefulShutdown_ExitCode verifies that serveWithShutdown returns nil
// (equivalent to exit code 0) when a SIGTERM is received.
func TestGracefulShutdown_ExitCode(t *testing.T) {
	port := freePort(t)
	httpSrv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	}
	wp := noopWorkerPool()
	emosCh := noopEmosCh()

	sigCh := make(chan os.Signal, 1)
	sendSignalAfter(sigCh, 20*time.Millisecond)

	err := serveWithShutdown(httpSrv, 0, wp, noopStaleJob(), noopRetentionJob(), emosCh, sigCh, time.Second, shutdownLog())
	if err != nil {
		t.Errorf("want nil (exit 0), got: %v", err)
	}
}

// TestGracefulShutdown_NoNewRequests verifies that after serveWithShutdown
// returns, the TCP port is no longer accepting connections.
func TestGracefulShutdown_NoNewRequests(t *testing.T) {
	port := freePort(t)
	httpSrv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
	}
	wp := noopWorkerPool()
	emosCh := noopEmosCh()

	sigCh := make(chan os.Signal, 1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveWithShutdown(httpSrv, 0, wp, noopStaleJob(), noopRetentionJob(), emosCh, sigCh, time.Second, shutdownLog()) //nolint:errcheck
	}()

	// Wait until the server is actually accepting connections.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Send shutdown signal and wait for serveWithShutdown to return.
	sigCh <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serveWithShutdown did not return within 3s")
	}

	// Now the port must refuse connections.
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Error("want connection refused after shutdown, got a connection")
	}
}

// TestGracefulShutdown_InFlightCompletes verifies that a request that is
// in-flight when the shutdown signal arrives still receives a 200 response
// (not dropped).
func TestGracefulShutdown_InFlightCompletes(t *testing.T) {
	port := freePort(t)

	// gate blocks the handler until the test releases it.
	gate := make(chan struct{})
	httpSrv := &http.Server{
		Addr: fmt.Sprintf("127.0.0.1:%d", port),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-gate
			w.WriteHeader(http.StatusOK)
		}),
	}
	wp := noopWorkerPool()
	emosCh := noopEmosCh()
	sigCh := make(chan os.Signal, 1)

	go serveWithShutdown(httpSrv, 0, wp, noopStaleJob(), noopRetentionJob(), emosCh, sigCh, 5*time.Second, shutdownLog()) //nolint:errcheck

	// Wait for server to be ready.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Start an in-flight request (will block on gate).
	respCh := make(chan int, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Get("http://" + addr + "/") //nolint:noctx
		if err != nil {
			respCh <- -1
			return
		}
		resp.Body.Close()
		respCh <- resp.StatusCode
	}()

	// Give the request time to reach the handler.
	time.Sleep(30 * time.Millisecond)

	// Send shutdown — server should drain in-flight requests.
	sigCh <- syscall.SIGTERM

	// Release the handler gate so the in-flight request can complete.
	time.Sleep(10 * time.Millisecond)
	close(gate)

	wg.Wait()
	code := <-respCh
	if code != http.StatusOK {
		t.Errorf("in-flight request: want 200, got %d", code)
	}
}

// TestGracefulShutdown_DrainChannel verifies that all items enqueued before
// the shutdown signal are flushed to the database before serveWithShutdown
// returns. Requires a real Postgres instance (TEST_DB_URL).
func TestGracefulShutdown_DrainChannel(t *testing.T) {
	dbURL := os.Getenv("TEST_DB_URL")
	if dbURL == "" {
		t.Skip("TEST_DB_URL not set; skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect db: %v", err)
	}
	defer pool.Close()

	// Apply migrations.
	for _, f := range []string{
		"../../migrations/000001_initial_schema.up.sql",
		"../../migrations/000002_add_external_id.up.sql",
	} {
		data, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			t.Fatalf("read migration %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			t.Logf("migration %s: %v (may be harmless)", f, err)
		}
	}

	// Seed org + app.
	var orgID int64
	pool.QueryRow(ctx, `INSERT INTO organizations (name) VALUES ('shutdown-org') RETURNING id`).Scan(&orgID)
	var appID int64
	pool.QueryRow(ctx, `INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'shutdown-app', 'hash') RETURNING id`, orgID).Scan(&appID)

	// Miniredis for cache.
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	// Real flusher + worker pool with slow flush interval so items stay buffered.
	flusher := ingest.New(pool, rdb, shutdownLog(), t.TempDir())
	wp := worker.New(worker.Config{
		WorkerCount:     2,
		BatchSize:       500,
		FlushIntervalMs: 10000, // very long — force drain-on-shutdown path
		ChannelCap:      1000,
	}, flusher.Flush, shutdownLog())

	// Enqueue 200 payloads.
	const n = 200
	appIDStr := fmt.Sprintf("%d", appID)
	baseTS := time.Now().UnixMilli()
	for i := range n {
		p := model.IngestPayload{
			AppID:        appIDStr,
			UserID:       "user-drain",
			ConferenceID: "conf-drain",
			SessionID:    fmt.Sprintf("sess-%d", i),
			ConnectionID: fmt.Sprintf("conn-%d", i),
			Events:       []model.StatSnapshot{{TS: baseTS + int64(i), RTTMs: float64(i)}},
		}
		if err := wp.Enqueue(p); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	// Minimal HTTP server (not used for requests in this test).
	port := freePort(t)
	httpSrv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	}
	emosCh := noopEmosCh()
	sigCh := make(chan os.Signal, 1)

	// Wait for server to start, then send SIGTERM.
	go func() {
		time.Sleep(50 * time.Millisecond)
		sigCh <- syscall.SIGTERM
	}()

	if err := serveWithShutdown(httpSrv, 0, wp, noopStaleJob(), noopRetentionJob(), emosCh, sigCh, time.Second, shutdownLog()); err != nil {
		t.Fatalf("serveWithShutdown: %v", err)
	}

	// All 200 rows must be in connection_stats.
	var count int
	pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM connection_stats`).Scan(&count)
	if count != n {
		t.Errorf("want %d rows in connection_stats after drain, got %d", n, count)
	}
}
