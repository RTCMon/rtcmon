package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/stale"
	"github.com/RTCMon/rtcmon/internal/worker"
)

// serveWithShutdown starts httpSrv and blocks until a signal arrives on sigCh
// or the server errors. On signal it:
//  1. Drains in-flight HTTP requests (httpShutdownTimeout, typically 30s)
//  2. Calls wp.Shutdown() to flush all buffered worker batches to DB
//  3. Calls staleJob.Shutdown() to stop the stale-conference background job
//  4. Closes emosCh to stop the eMOS goroutine cleanly
//
// Returns nil on clean shutdown, non-nil on unexpected server startup error.
//
// Shutdown order is intentional: HTTP stops accepting new enqueues first, then
// the worker pool flushes everything already buffered, then the stale job stops
// (so its last eMOS triggers can still be queued), then eMOS exits.
func serveWithShutdown(
	httpSrv *http.Server,
	wp *worker.Pool,
	staleJob *stale.Job,
	emosCh chan int64,
	sigCh <-chan os.Signal,
	httpShutdownTimeout time.Duration,
	log *logrus.Logger,
) error {
	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		} else {
			errCh <- nil
		}
	}()

	select {
	case sig := <-sigCh:
		log.WithField("signal", sig).Info("ingest-api: shutdown signal received")
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	}

	// Step 1: stop accepting new requests; let in-flight ones complete.
	shutCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		log.WithError(err).Warn("ingest-api: http server forced close after timeout")
	}

	// Step 2: drain the worker pool — blocks until every goroutine has flushed.
	wp.Shutdown()

	// Step 3: stop the stale-conference job — must happen before emosCh is
	// closed so any in-flight eMOS triggers can still be queued.
	staleJob.Shutdown()

	// Step 4: stop the eMOS goroutine.
	close(emosCh)

	log.Info("ingest-api: shutdown complete")
	return nil
}
