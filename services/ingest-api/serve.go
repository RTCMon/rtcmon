package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/metrics"
	"github.com/RTCMon/rtcmon/internal/retention"
	"github.com/RTCMon/rtcmon/internal/stale"
	"github.com/RTCMon/rtcmon/internal/worker"
)

// serveWithShutdown starts httpSrv and a Prometheus metrics server, then
// blocks until a signal arrives on sigCh or the main server errors. On signal:
//  1. Drains in-flight HTTP requests (httpShutdownTimeout, typically 30s)
//  2. Calls wp.Shutdown() to flush all buffered worker batches to DB
//  3. Calls staleJob.Shutdown() to stop the stale-conference background job
//  4. Calls retentionJob.Shutdown() to stop the data-retention cleanup job
//  5. Closes emosCh to stop the eMOS goroutine cleanly
//  6. Stops the metrics server (5 s timeout)
//
// Returns nil on clean shutdown, non-nil on unexpected server startup error.
func serveWithShutdown(
	httpSrv *http.Server,
	metricsPort int,
	wp *worker.Pool,
	staleJob *stale.Job,
	retentionJob *retention.Job,
	emosCh chan int64,
	sigCh <-chan os.Signal,
	httpShutdownTimeout time.Duration,
	log *logrus.Logger,
) error {
	// Start the internal metrics server on metricsPort.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	metricsSrv := &http.Server{
		Addr:    fmt.Sprintf(":%d", metricsPort),
		Handler: metricsMux,
	}
	go func() {
		if err := metricsSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) && err != nil {
			log.WithError(err).Warn("ingest-api: metrics server error")
		}
	}()
	log.WithField("port", metricsPort).Info("ingest-api: metrics server listening")

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

	// Step 4: stop the data-retention cleanup job.
	retentionJob.Shutdown()

	// Step 5: stop the eMOS goroutine.
	close(emosCh)

	// Step 6: stop the metrics server.
	mCtx, mCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer mCancel()
	if err := metricsSrv.Shutdown(mCtx); err != nil {
		log.WithError(err).Warn("ingest-api: metrics server forced close")
	}

	log.Info("ingest-api: shutdown complete")
	return nil
}
