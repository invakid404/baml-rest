package worker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// NewMetricsRegistry returns a fresh Prometheus registry pre-populated
// with the Go runtime + process collectors. The default subprocess worker
// reads metrics from this registry via GetMetrics; keeping construction
// here means in-process callers don't need to mirror the collector list.
func NewMetricsRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}
