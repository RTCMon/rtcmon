// Package worker implements an in-process fan-out worker pool that buffers
// ingest payloads and flushes them in batches. Multiple goroutines read from
// a single buffered channel, each accumulating its own local batch to avoid
// shared-state contention.
package worker

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/metrics"
	"github.com/RTCMon/rtcmon/internal/model"
)

// ErrChannelFull is returned by Enqueue when the internal buffer is at
// capacity or when the pool has been shut down.
var ErrChannelFull = errors.New("worker: channel is full")

// FlushFn is called with each batch of payloads ready to be persisted.
// The context is context.Background() for the log stub; BE-011 will use it
// for DB operation timeouts.
type FlushFn func(ctx context.Context, batch []model.IngestPayload)

// Config holds worker pool tuning parameters.
type Config struct {
	WorkerCount     int
	BatchSize       int
	FlushIntervalMs int
	ChannelCap      int
}

// Pool is a goroutine-safe in-process worker pool. Create with New; stop with
// Shutdown. Do not copy after first use.
type Pool struct {
	ch      chan model.IngestPayload
	flushFn FlushFn
	cfg     Config
	log     *logrus.Logger

	wg   sync.WaitGroup
	once sync.Once

	// mu guards closed and the channel send in Enqueue, preventing a concurrent
	// Shutdown from closing the channel while a send is in progress.
	mu     sync.RWMutex
	closed bool
}

// New creates a Pool and starts cfg.WorkerCount goroutines immediately.
// The pool is ready to accept items as soon as New returns.
func New(cfg Config, flushFn FlushFn, log *logrus.Logger) *Pool {
	p := &Pool{
		ch:      make(chan model.IngestPayload, cfg.ChannelCap),
		flushFn: flushFn,
		cfg:     cfg,
		log:     log,
	}

	for range cfg.WorkerCount {
		p.wg.Add(1)
		go p.run()
	}

	return p
}

// Enqueue submits a payload for async processing. It returns ErrChannelFull
// immediately (non-blocking) when the buffer is at capacity or the pool is
// shut down.
func (p *Pool) Enqueue(payload model.IngestPayload) error {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.closed {
		return ErrChannelFull
	}

	select {
	case p.ch <- payload:
		metrics.IngestChannelDepth.Set(float64(len(p.ch)))
		return nil
	default:
		return ErrChannelFull
	}
}

// Shutdown signals all worker goroutines to drain remaining items, waits for
// every goroutine to flush and exit, then returns. Safe to call multiple times.
func (p *Pool) Shutdown() {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		close(p.ch)
		p.mu.Unlock()
	})
	p.wg.Wait()
}

// run is the main loop executed by each worker goroutine. It reads from the
// shared channel and accumulates a local batch, flushing when the batch
// reaches BatchSize or the ticker fires.
func (p *Pool) run() {
	defer p.wg.Done()

	batch := make([]model.IngestPayload, 0, p.cfg.BatchSize)
	ticker := time.NewTicker(time.Duration(p.cfg.FlushIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case item, ok := <-p.ch:
			if !ok {
				// Channel closed — drain complete. Flush any remaining items.
				if len(batch) > 0 {
					p.doFlush(batch)
				}
				return
			}
			batch = append(batch, item)
			if len(batch) >= p.cfg.BatchSize {
				p.doFlush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				p.doFlush(batch)
				batch = batch[:0]
			}
		}
	}
}

func (p *Pool) doFlush(batch []model.IngestPayload) {
	start := time.Now()
	p.flushFn(context.Background(), batch)
	dur := time.Since(start)

	size := len(batch)
	metrics.WorkerFlushDuration.Observe(dur.Seconds())
	metrics.WorkerFlushBatchSize.Observe(float64(size))
	metrics.IngestChannelDepth.Set(float64(len(p.ch)))

	if p.log != nil {
		p.log.WithFields(logrus.Fields{
			"batch_size":  size,
			"duration_ms": dur.Milliseconds(),
			"channel_depth": len(p.ch),
		}).Info("worker: flushed batch")
	}
}
