package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"

	"github.com/invakid404/baml-rest/internal/artifactprofile"
	"github.com/invakid404/baml-rest/pool"
)

// The worker->host metrics bridge, and specifically what it does when a worker's
// metric families cannot be decoded.
//
// de-BAML serving cutover S2 exports the worker's artifact profile and its
// expected-profile violation through this bridge, and the wrong-artifact alert has
// no other input. The bridge used to `continue` past an undecodable family, which
// would serve a 200 OK scrape with the alert's input missing — a false green
// precisely when something is already wrong. These tests pin the happy path and
// the failure path together, because only the pair shows the failure path is not
// simply "everything fails".

// fakeWorkerMetrics is a worker metrics source returning canned payloads.
type fakeWorkerMetrics struct {
	metrics []pool.WorkerMetrics
}

func (f fakeWorkerMetrics) GatherWorkerMetrics(context.Context) []pool.WorkerMetrics {
	return f.metrics
}

// artifactProfileFamilyBytes marshals a worker-side S2 artifact-profile family,
// exactly as a real worker's metrics RPC returns it.
func artifactProfileFamilyBytes(t *testing.T) []byte {
	t.Helper()
	reg := prometheus.NewRegistry()
	att, err := artifactprofile.Attest(artifactprofile.ProfileNativeCapable, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if err := artifactprofile.Register(reg, att); err != nil {
		t.Fatalf("Register: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != artifactprofile.ArtifactInfoMetric {
			continue
		}
		raw, err := proto.Marshal(mf)
		if err != nil {
			t.Fatalf("marshal family: %v", err)
		}
		return raw
	}
	t.Fatalf("%s was not gathered", artifactprofile.ArtifactInfoMetric)
	return nil
}

// TestMetricsBridgeCarriesTheWorkerArtifactProfile is the happy path: a worker's
// artifact-profile series reaches the combined scrape, prefixed and tagged with
// the worker's process label. This is the S2 alert's actual data path.
func TestMetricsBridgeCarriesTheWorkerArtifactProfile(t *testing.T) {
	g := &combinedMetricsGatherer{
		prefix:       "bamlrest_",
		mainGatherer: prometheus.NewRegistry(),
		pool: fakeWorkerMetrics{metrics: []pool.WorkerMetrics{{
			WorkerID:       3,
			MetricFamilies: [][]byte{artifactProfileFamilyBytes(t)},
		}}},
		logger: zerolog.New(&bytes.Buffer{}),
	}

	families, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	want := "bamlrest_" + artifactprofile.ArtifactInfoMetric
	var found *dto.MetricFamily
	for _, mf := range families {
		if mf.GetName() == want {
			found = mf
		}
	}
	if found == nil {
		t.Fatalf("combined scrape does not carry %q; the S2 wrong-artifact alert would have no input", want)
	}
	if len(found.Metric) != 1 {
		t.Fatalf("%q has %d samples, want 1", want, len(found.Metric))
	}
	var process, profile string
	for _, lp := range found.Metric[0].Label {
		switch lp.GetName() {
		case "process":
			process = lp.GetValue()
		case "profile":
			profile = lp.GetValue()
		}
	}
	if process != "worker_3" {
		t.Errorf("process label = %q, want %q", process, "worker_3")
	}
	if profile != string(artifactprofile.ProfileNativeCapable) {
		t.Errorf("profile label = %q, want %q", profile, artifactprofile.ProfileNativeCapable)
	}
}

// TestMetricsBridgeFailsTheScrapeOnAnUndecodableWorkerFamily is the mutation the
// review asked for: a worker returning a family the host cannot decode must FAIL
// the scrape (the /metrics handler turns a Gather error into a 500), log the
// failure, and count it — not quietly return the rest.
func TestMetricsBridgeFailsTheScrapeOnAnUndecodableWorkerFamily(t *testing.T) {
	before := testutil.ToFloat64(workerMetricsBridgeFailures.WithLabelValues("decode"))

	var logs bytes.Buffer
	g := &combinedMetricsGatherer{
		prefix:       "bamlrest_",
		mainGatherer: prometheus.NewRegistry(),
		pool: fakeWorkerMetrics{metrics: []pool.WorkerMetrics{{
			WorkerID: 7,
			MetricFamilies: [][]byte{
				artifactProfileFamilyBytes(t),
				// Not a valid dto.MetricFamily encoding.
				[]byte("\xff\xff\xff\xffnot-a-metric-family"),
			},
		}}},
		logger: zerolog.New(&logs),
	}

	families, err := g.Gather()
	if err == nil {
		t.Fatalf("Gather returned success with an undecodable worker family; it served %d families as if complete", len(families))
	}
	if families != nil {
		t.Errorf("Gather returned families alongside its error; a partial aggregate must not be presentable")
	}
	if !strings.Contains(err.Error(), "worker 7") {
		t.Errorf("error %q does not name the failing worker", err)
	}

	if after := testutil.ToFloat64(workerMetricsBridgeFailures.WithLabelValues("decode")); after != before+1 {
		t.Errorf("bridge failure counter = %v, want %v", after, before+1)
	}
	if !strings.Contains(logs.String(), "worker metrics bridge") {
		t.Errorf("no bridge-failure log line:\n%s", logs.String())
	}
}

// TestMetricsBridgeFailsTheScrapeWhenAHealthyWorkersMetricsRPCFails is the second
// half of the same rule, and the one a cold review had to elevate: the pool used
// to log a warning and DROP a healthy worker whose metrics RPC failed, so the
// host never saw a record for it and the decode-failure path above could never
// fire. The scrape then returned 200 with that worker's artifact-profile series
// simply absent — which reads exactly like "that worker has nothing to report".
//
// A wrong-profile worker that is still healthy for requests but whose metrics RPC
// fails would therefore be HIDDEN from the alert that exists to catch it. The pool
// now returns an Err-bearing record, and the scrape fails on it.
func TestMetricsBridgeFailsTheScrapeWhenAHealthyWorkersMetricsRPCFails(t *testing.T) {
	before := testutil.ToFloat64(workerMetricsBridgeFailures.WithLabelValues("rpc"))

	var logs bytes.Buffer
	g := &combinedMetricsGatherer{
		prefix:       "bamlrest_",
		mainGatherer: prometheus.NewRegistry(),
		pool: fakeWorkerMetrics{metrics: []pool.WorkerMetrics{
			// One worker answered; the other is healthy but unreachable for
			// metrics. Serving only the first is the false green.
			{WorkerID: 1, MetricFamilies: [][]byte{artifactProfileFamilyBytes(t)}},
			{WorkerID: 4, Err: errors.New("rpc error: code = Unavailable desc = worker crashed")},
		}},
		logger: zerolog.New(&logs),
	}

	families, err := g.Gather()
	if err == nil {
		t.Fatalf("Gather returned success while a healthy worker's metrics RPC had failed; it served %d families as if complete", len(families))
	}
	if families != nil {
		t.Errorf("Gather returned families alongside its error; a partial aggregate must not be presentable")
	}
	if !strings.Contains(err.Error(), "worker 4") {
		t.Errorf("error %q does not name the unreachable worker", err)
	}

	if after := testutil.ToFloat64(workerMetricsBridgeFailures.WithLabelValues("rpc")); after != before+1 {
		t.Errorf("rpc bridge failure counter = %v, want %v", after, before+1)
	}
	if !strings.Contains(logs.String(), "metrics RPC failed") {
		t.Errorf("no metrics-RPC failure log line:\n%s", logs.String())
	}
}

// TestMetricsBridgeStillServesWithNoWorkers pins that the failure path above is
// keyed on an actual decode failure, not on the mere presence of the worker
// branch: an empty pool still produces a successful scrape.
func TestMetricsBridgeStillServesWithNoWorkers(t *testing.T) {
	g := &combinedMetricsGatherer{
		prefix:       "bamlrest_",
		mainGatherer: prometheus.NewRegistry(),
		pool:         fakeWorkerMetrics{},
		logger:       zerolog.New(&bytes.Buffer{}),
	}
	if _, err := g.Gather(); err != nil {
		t.Fatalf("Gather with no workers: %v", err)
	}
}
