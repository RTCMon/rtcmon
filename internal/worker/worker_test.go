package worker

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"go.uber.org/goleak"

	"github.com/RTCMon/rtcmon/internal/model"
)

func newTestLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

// collectedFlush returns a FlushFn that appends all received items into a
// mutex-protected slice, and a count() func that returns the current total.
func collectedFlush() (FlushFn, func() int) {
	var mu sync.Mutex
	var items []model.IngestPayload

	fn := func(_ context.Context, batch []model.IngestPayload) {
		mu.Lock()
		items = append(items, batch...)
		mu.Unlock()
	}

	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(items)
	}

	return fn, count
}

func payload() model.IngestPayload {
	return model.IngestPayload{
		ConferenceID: "conf-1",
		SessionID:    "sess-1",
		ConnectionID: "conn-1",
		Events:       []model.StatSnapshot{{TS: 1_000_000}},
	}
}

// TestWorker_FlushOnTicker verifies that a single enqueued item is flushed
// within flushInterval + slack, without waiting for the batch to fill.
func TestWorker_FlushOnTicker(t *testing.T) {
	flush, count := collectedFlush()
	p := New(Config{
		WorkerCount:     1,
		BatchSize:       500,
		FlushIntervalMs: 50,
		ChannelCap:      1000,
	}, flush, newTestLogger())
	defer p.Shutdown()

	if err := p.Enqueue(payload()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	time.Sleep(80 * time.Millisecond)

	if got := count(); got != 1 {
		t.Errorf("want 1 item flushed after ticker, got %d", got)
	}
}

// TestWorker_FlushOnBatchFull verifies that filling the batch triggers an
// immediate flush without waiting for the ticker.
func TestWorker_FlushOnBatchFull(t *testing.T) {
	const batchSize = 5
	flush, count := collectedFlush()
	p := New(Config{
		WorkerCount:     1,
		BatchSize:       batchSize,
		FlushIntervalMs: 500, // long ticker — must not influence this test
		ChannelCap:      1000,
	}, flush, newTestLogger())
	defer p.Shutdown()

	for range batchSize {
		if err := p.Enqueue(payload()); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	// Flush must happen well before the 500ms ticker.
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if count() == batchSize {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Errorf("want %d items flushed on batch full, got %d after 50ms", batchSize, count())
}

// TestWorker_EnqueueChannelFull verifies that Enqueue returns ErrChannelFull
// immediately when the channel buffer is exhausted.
func TestWorker_EnqueueChannelFull(t *testing.T) {
	// Use a slow flush and tiny cap so we can fill the channel before it drains.
	var blocked atomic.Bool
	blockFn := func(_ context.Context, _ []model.IngestPayload) {
		// Block flush until the test asserts ErrChannelFull.
		for blocked.Load() {
			time.Sleep(time.Millisecond)
		}
	}

	p := New(Config{
		WorkerCount:     1,
		BatchSize:       1,   // flush immediately on first item
		FlushIntervalMs: 500, // long ticker
		ChannelCap:      2,
	}, blockFn, newTestLogger())
	defer func() {
		blocked.Store(false) // unblock flush so Shutdown can drain
		p.Shutdown()
	}()

	blocked.Store(true)

	// Fill the channel. The first item goes to the worker immediately (batch=1
	// triggers a flush that blocks), so we can then fill the 2-slot buffer.
	_ = p.Enqueue(payload()) // picked up by worker, starts blocking flush
	time.Sleep(5 * time.Millisecond) // let the worker pick it up

	_ = p.Enqueue(payload()) // fills slot 1
	_ = p.Enqueue(payload()) // fills slot 2

	// Channel is now at capacity.
	if err := p.Enqueue(payload()); err != ErrChannelFull {
		t.Errorf("want ErrChannelFull, got %v", err)
	}
}

// TestWorker_Shutdown_DrainsChannel verifies that Shutdown flushes all items
// that were enqueued before the call.
func TestWorker_Shutdown_DrainsChannel(t *testing.T) {
	const n = 50
	flush, count := collectedFlush()
	p := New(Config{
		WorkerCount:     1,
		BatchSize:       500, // batch larger than n — forces drain-on-close path
		FlushIntervalMs: 500, // long ticker — must not fire during test
		ChannelCap:      1000,
	}, flush, newTestLogger())

	for range n {
		if err := p.Enqueue(payload()); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	p.Shutdown()

	if got := count(); got != n {
		t.Errorf("want %d items after Shutdown, got %d", n, got)
	}
}

// TestWorker_NoGoroutineLeak verifies that all goroutines exit after Shutdown.
func TestWorker_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	flush, _ := collectedFlush()
	p := New(Config{
		WorkerCount:     4,
		BatchSize:       500,
		FlushIntervalMs: 50,
		ChannelCap:      1000,
	}, flush, newTestLogger())

	p.Shutdown()
}

// TestWorker_ConcurrentEnqueue verifies goroutine safety: 100 concurrent
// producers each enqueue 10 items; all 1000 must reach flush after Shutdown.
func TestWorker_ConcurrentEnqueue(t *testing.T) {
	const (
		producers  = 100
		perProducer = 10
		total      = producers * perProducer
	)

	flush, count := collectedFlush()
	p := New(Config{
		WorkerCount:     4,
		BatchSize:       500,
		FlushIntervalMs: 50,
		ChannelCap:      total * 2, // large enough that no ErrChannelFull occurs
	}, flush, newTestLogger())

	var wg sync.WaitGroup
	for range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perProducer {
				if err := p.Enqueue(payload()); err != nil {
					t.Errorf("enqueue: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	p.Shutdown()

	if got := count(); got != total {
		t.Errorf("want %d items after concurrent enqueue, got %d", total, got)
	}
}
