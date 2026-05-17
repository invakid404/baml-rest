package main

import (
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
	"github.com/invakid404/baml-rest/internal/worker"
)

// workerModeRuntimeConfig is the resolved runtime configuration
// cmd/serve threads into configureWorkerMode. Lives in a shared
// (non-tag-split) file so both worker_mode_inprocess.go and
// worker_mode_subprocess.go can name the type; the in-process build
// reads every field, and the subprocess build accepts (and ignores)
// it so the two builds share a signature.
//
// Resolved exactly once at server startup and reused for every
// handler in the pool — for the standard server binaries this means
// every in-process handler observes the same env-derived values
// cmd/worker would have observed at its own startup.
type workerModeRuntimeConfig struct {
	// Runtime is the generated-runtime wrapper handlers consume for
	// dispatch (Method / ParseMethod / MakeAdapter). cmd/serve uses
	// internal/rootruntime.Runtime{}.
	Runtime worker.Runtime

	// BuildRequest mirrors the BAML_REST_USE_BUILD_REQUEST /
	// BAML_REST_DISABLE_CALL_BUILD_REQUEST envs as a typed config the
	// generated router reads through adapter.BuildRequestConfig().
	BuildRequest bamlutils.BuildRequestConfig

	// BaseURLRewrites is the URL rewrite ruleset the worker applies
	// before SetClientRegistry. Outbound HTTP rewrites go through
	// HTTPClient's per-client rules below — populated from the same
	// source so both seams stay in lockstep.
	BaseURLRewrites []urlrewrite.Rule

	// HTTPClient is the per-handler llmhttp.Client every BuildRequest
	// dispatch reads via adapter.HTTPClient(). Built with the tuned
	// defaultLLMTransport plus BaseURLRewrites.
	HTTPClient *llmhttp.Client
}
