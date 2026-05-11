// Package metrics holds the shared Prometheus registry and all metric
// definitions for both services. Both ingest-api and query-api import this
// package so they share the same set of metric objects; each service only
// exposes the subset of metrics it populates.
package metrics

import "github.com/prometheus/client_golang/prometheus"

// Registry is the single Prometheus registry for the process. Use this
// instead of prometheus.DefaultRegisterer so tests can create isolated
// registries without touching global state.
var Registry = prometheus.NewRegistry()

// Ingest API metrics.
var (
	// IngestRequestsTotal counts completed HTTP requests to the ingest API.
	// Label: status = "2xx" | "4xx" | "5xx"
	IngestRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ingest_requests_total",
		Help: "Total number of HTTP requests handled by the ingest API, by status class.",
	}, []string{"status"})

	// IngestChannelDepth is the current number of payloads waiting in the
	// worker pool channel. Updated on every Enqueue and after every flush.
	IngestChannelDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ingest_channel_depth",
		Help: "Current number of payloads buffered in the worker pool channel.",
	})

	// WorkerFlushDuration measures wall-clock time of each DB flush call.
	WorkerFlushDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "worker_flush_duration_seconds",
		Help:    "Time taken for each worker pool DB flush.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.0},
	})

	// WorkerFlushBatchSize measures the number of payloads per flush.
	WorkerFlushBatchSize = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "worker_flush_batch_size",
		Help:    "Number of payloads per worker pool flush.",
		Buckets: []float64{1, 10, 50, 100, 250, 500},
	})
)

// Query API metrics.
var (
	// ConferencesTotal counts newly created conferences by app_id.
	// Label: app_id
	ConferencesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "conferences_total",
		Help: "Total number of new conferences created, by app_id.",
	}, []string{"app_id"})

	// ObservationsTriggeredTotal counts observation rule firings by rule name.
	// Label: rule
	ObservationsTriggeredTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "observations_triggered_total",
		Help: "Total number of observations triggered by the observations engine, by rule name.",
	}, []string{"rule"})
)

func init() {
	Registry.MustRegister(
		IngestRequestsTotal,
		IngestChannelDepth,
		WorkerFlushDuration,
		WorkerFlushBatchSize,
		ConferencesTotal,
		ObservationsTriggeredTotal,
	)
}
