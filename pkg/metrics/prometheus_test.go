package metrics

import "testing"

// TestRegisterPrometheusMetricsIsIdempotent guards the sync.Once in
// RegisterPrometheusMetrics. prometheus.MustRegister panics on a duplicate collector, and
// Serve registers the collectors on startup in addition to the call made by the command
// wiring, so a second call has to be a no-op rather than a crash.
func TestRegisterPrometheusMetricsIsIdempotent(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RegisterPrometheusMetrics panicked when called twice: %v", r)
		}
	}()

	RegisterPrometheusMetrics()
	RegisterPrometheusMetrics()
}
