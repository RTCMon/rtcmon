package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMetrics_AllRegistered(t *testing.T) {
	// Gather verifies that all registered metrics can produce valid output.
	mfs, err := Registry.Gather()
	if err != nil {
		t.Fatalf("Registry.Gather: %v", err)
	}

	want := map[string]bool{
		"ingest_requests_total":        false,
		"ingest_channel_depth":         false,
		"worker_flush_duration_seconds": false,
		"worker_flush_batch_size":      false,
		"conferences_total":            false,
		"observations_triggered_total": false,
	}

	// Metrics with no observations produce no metric families, so touch each
	// one to ensure it appears in Gather output.
	IngestRequestsTotal.WithLabelValues("2xx").Add(0)
	IngestChannelDepth.Set(0)
	WorkerFlushDuration.(prometheus.Histogram).Observe(0)
	WorkerFlushBatchSize.(prometheus.Histogram).Observe(0)
	ConferencesTotal.WithLabelValues("1").Add(0)
	ObservationsTriggeredTotal.WithLabelValues("packet_loss_spike").Add(0)

	mfs, err = Registry.Gather()
	if err != nil {
		t.Fatalf("Registry.Gather after touch: %v", err)
	}

	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}

	for name, found := range want {
		if !found {
			t.Errorf("metric %q not found in registry", name)
		}
	}
}
