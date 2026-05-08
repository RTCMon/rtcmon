package ingest

import (
	"context"
	"io"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/model"
)

// newMiniredis starts an in-process Redis and returns a connected client.
func newMiniredis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func discardLog() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

// newCacheFlusher creates a Flusher with a nil pool (DB not needed for cache
// tests that call updateCache directly).
func newCacheFlusher(rdb redisClient) *Flusher {
	return &Flusher{
		rdb:       rdb,
		log:       discardLog(),
		ewmaState: make(map[string]*ewmaState),
	}
}

func cachePayload(appID, confID, userID string, ts int64) model.IngestPayload {
	return model.IngestPayload{
		AppID:        appID,
		UserID:       userID,
		ConferenceID: confID,
		SessionID:    "sess-1",
		ConnectionID: "conn-1",
		Events:       []model.StatSnapshot{{TS: ts}},
	}
}

// ── Tests ────────────────────────────────────────────────────────────────────

// TestCache_KeyCreatedAfterFlush verifies that updateCache creates the Redis
// hash key with the expected fields.
func TestCache_KeyCreatedAfterFlush(t *testing.T) {
	_, rdb := newMiniredis(t)
	f := newCacheFlusher(rdb)
	ctx := context.Background()

	batch := []model.IngestPayload{
		cachePayload("1", "conf-1", "user-1", 1000),
	}
	f.updateCache(ctx, batch)

	vals, err := rdb.HGetAll(ctx, "active_conf:1:conf-1").Result()
	if err != nil {
		t.Fatalf("HGetAll: %v", err)
	}
	if vals["last_event_ts"] == "" {
		t.Error("want last_event_ts field, got none")
	}
	if vals["participant_count"] == "" {
		t.Error("want participant_count field, got none")
	}
}

// TestCache_TTLReset verifies that a second flush resets the TTL back to ~600s
// even after some time has elapsed.
func TestCache_TTLReset(t *testing.T) {
	mr, rdb := newMiniredis(t)
	f := newCacheFlusher(rdb)
	ctx := context.Background()

	batch := []model.IngestPayload{cachePayload("1", "conf-ttl", "user-1", 1000)}

	f.updateCache(ctx, batch)

	// Simulate 5 seconds passing — TTL should now be ~595s.
	mr.FastForward(5 * time.Second)

	// Second flush must reset TTL back to 600s.
	f.updateCache(ctx, batch)

	ttl, err := rdb.TTL(ctx, "active_conf:1:conf-ttl").Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl < 595*time.Second {
		t.Errorf("want TTL ≥ 595s after reset, got %v", ttl)
	}
}

// TestCache_LastEventTs verifies that last_event_ts is the maximum ts in the
// batch, not the first or last enqueued value.
func TestCache_LastEventTs(t *testing.T) {
	_, rdb := newMiniredis(t)
	f := newCacheFlusher(rdb)
	ctx := context.Background()

	batch := []model.IngestPayload{
		{
			AppID: "1", UserID: "user-1", ConferenceID: "conf-ts",
			SessionID: "s", ConnectionID: "c",
			Events: []model.StatSnapshot{
				{TS: 1000},
				{TS: 3000},
				{TS: 2000},
			},
		},
	}
	f.updateCache(ctx, batch)

	raw, err := rdb.HGet(ctx, "active_conf:1:conf-ts", "last_event_ts").Result()
	if err != nil {
		t.Fatalf("HGet: %v", err)
	}
	got, _ := strconv.ParseInt(raw, 10, 64)
	if got != 3000 {
		t.Errorf("want last_event_ts=3000, got %d", got)
	}
}

// TestCache_RedisFailureDoesNotFailFlush verifies that a Redis error is swallowed
// and does not panic or propagate.
func TestCache_RedisFailureDoesNotFailFlush(t *testing.T) {
	mr, rdb := newMiniredis(t)
	f := newCacheFlusher(rdb)
	ctx := context.Background()

	// Close miniredis to simulate a Redis outage.
	mr.Close()

	batch := []model.IngestPayload{cachePayload("1", "conf-fail", "user-1", 1000)}

	// Must not panic.
	f.updateCache(ctx, batch)
}

// ── Pipeline counting helpers ─────────────────────────────────────────────────

// countingPipeliner wraps a real redis.Pipeliner and counts Exec calls.
type countingPipeliner struct {
	redis.Pipeliner
	execCount *atomic.Int32
}

func (cp *countingPipeliner) Exec(ctx context.Context) ([]redis.Cmder, error) {
	cp.execCount.Add(1)
	return cp.Pipeliner.Exec(ctx)
}

// countingRedisClient wraps a redisClient and injects the counting pipeliner.
type countingRedisClient struct {
	inner     redisClient
	execCount atomic.Int32
}

func (c *countingRedisClient) Pipeline() redis.Pipeliner {
	return &countingPipeliner{
		Pipeliner: c.inner.Pipeline(),
		execCount: &c.execCount,
	}
}

// TestCache_Pipeline_SingleRoundTrip verifies that updateCache calls pipeline
// Exec exactly once per batch, regardless of the number of conferences.
func TestCache_Pipeline_SingleRoundTrip(t *testing.T) {
	_, rdb := newMiniredis(t)
	counter := &countingRedisClient{inner: rdb}
	f := newCacheFlusher(counter)
	ctx := context.Background()

	// Three distinct conferences in one batch.
	batch := []model.IngestPayload{
		cachePayload("1", "conf-A", "user-1", 1000),
		cachePayload("1", "conf-B", "user-1", 2000),
		cachePayload("1", "conf-C", "user-1", 3000),
	}
	f.updateCache(ctx, batch)

	if got := counter.execCount.Load(); got != 1 {
		t.Errorf("want 1 pipeline Exec call, got %d", got)
	}
}
