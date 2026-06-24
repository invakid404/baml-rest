# baml-rest

## Embedding via `dynclient`

The dynamic endpoints (`/call/_dynamic`, `/stream/_dynamic`, etc.) are also
available as an in-process Go library at
`github.com/invakid404/baml-rest/dynclient` for projects that want to call
LLMs through baml-rest's BuildRequest pipeline without running a separate
server.

Install:

```sh
go get github.com/invakid404/baml-rest/dynclient@latest
```

Quickstart:

```go
import "github.com/invakid404/baml-rest/dynclient"

c, err := dynclient.New(
    dynclient.WithLogger(logger),
    dynclient.WithMetricsRegistry(reg),
)
// c.DynamicCall(ctx, req)
// c.DynamicStream(ctx, req)
```

See [`dynclient/`](dynclient/) for the full surface (10 options, 5 endpoint
methods, iterator-style streaming). The patched-BAML CFFI library is
bundled as a nested module — no separate setup required.

### HTTP backend selection and tuning

The underlying HTTP backend (net/http vs fasthttp) is chosen by
`WithClientMode` and tuned independently by `WithNetHTTPClient`
(net/http only) and `WithFastHTTPClient` (fasthttp only). Selection and
tuning are orthogonal axes: the default `llmhttp.ClientModeAuto` routes
each origin to the appropriate backend via an ALPN probe, and either
tuning option can apply in parallel.

```go
import (
    "net/http"

    "github.com/invakid404/baml-rest/bamlutils/llmhttp"
    "github.com/invakid404/baml-rest/dynclient"
)

netClient := &http.Client{
    Transport: http.DefaultTransport.(*http.Transport).Clone(),
}

// Force net/http with a custom *http.Client:
dynclient.New(
    dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
    dynclient.WithNetHTTPClient(netClient),
)

// Force net/http with the package's tuned default transport
// (no custom client needed):
dynclient.New(dynclient.WithClientMode(llmhttp.ClientModeNetHTTP))

// Keep Auto mode and tune fasthttp routes only:
dynclient.New(
    dynclient.WithFastHTTPClient(llmhttp.FastHTTPClientOptions{MaxConns: 1024}),
)

// Keep Auto mode and tune both backend families:
dynclient.New(
    dynclient.WithNetHTTPClient(netClient),
    dynclient.WithFastHTTPClient(llmhttp.FastHTTPClientOptions{MaxConns: 1024}),
)
```

Forced modes reject tuning aimed at the unused backend — e.g.
`WithClientMode(ClientModeNetHTTP)` combined with `WithFastHTTPClient`
is a hard error from `New` so the misconfiguration surfaces at startup
rather than as a silent no-op at request time.

`BAML_REST_HTTP_CLIENT` (`auto`/`fasthttp`/`nethttp`) is a server/worker
env var only; dynclient stays explicit-only and never reads it.

## Runtime configuration

baml-rest reads the following environment variables at startup:

- `BAML_REST_USE_BUILD_REQUEST` — toggle the BuildRequest/StreamRequest code
  path. `1`/`true`/`yes`/`on` enables it; any other value (including empty)
  falls back to the legacy CallStream+OnTick path. This is a full rollback:
  on BAML 0.219+ adapters, baml-rest's centralised round-robin (resolver,
  in-process coordinator, and worker SharedState broker / RemoteAdvancer)
  is also disengaged, and BAML's own runtime handles strategy rotation
  per-worker. Use this flag as an incident-response kill switch when
  anything in the new request path regresses.
- `BAML_REST_BASE_URL_REWRITES` — semicolon-separated URL rewrite rules
  applied to LLM provider base URLs, both at build time and at runtime.
  Format: `from1=to1;from2=to2`. See
  [bamlutils/urlrewrite](bamlutils/urlrewrite/urlrewrite.go) for the full
  semantics.
- `BAML_REST_CLIENT_DEFAULTS` — JSON object pinning deployment-wide defaults
  for BAML `ClientRegistry` option values. Each key in `options` is merged
  into every caller's `client_registry.clients[].options` map unless the
  caller already set it. Example:

  ```json
  {
    "client_defaults": {
      "options": {
        "allowed_role_metadata": ["cache_control"]
      }
    }
  }
  ```

  v1 recognizes `allowed_role_metadata` only. See
  [bamlutils/clientdefaults](bamlutils/clientdefaults/clientdefaults.go) for
  the merge contract, supported opt-outs, and supported-version caveats.
- `BAML_REST_PRESERVE_SCHEMA_ORDER_DEFAULT` — when truthy
  (`1`/`true`/`yes`/`on`), dynamic requests that omit or set
  `preserve_schema_order: null` inherit `true`, so the rendered
  `output_format` follows the JSON key order captured from `output_schema`.
  Per-request `preserve_schema_order: true` or `false` always wins. Default
  unset/false keeps the alphabetical ordering introduced for deterministic
  prompts.
- `BAML_LOG` — BAML internal log level (`debug`, `info`, `warn`, `error`).

## In-process build mode

By default, a from-source `go build` of `./cmd/serve` produces a single-process
binary with the BAML worker handler linked directly into the server. The
`subprocess` Go build tag opts back into the supervised pair of server +
`go-plugin` gRPC worker subprocesses; the embedded worker binary is then
extracted at startup. The project build wrapper (`cmd/build`) still defaults
to subprocess output for distributed deployments and exposes `--inprocess` to
flip back to the single-process layout.

### Building

- `go build ./cmd/serve` — default, single-process (in-process handler).
- `go build -tags=subprocess ./cmd/serve` — supervised pair, requires a
  prebuilt `cmd/worker` to embed.
- `go run ./cmd/build …` — distributed default; produces subprocess output.
- `go run ./cmd/build --inprocess …` (or `BAML_REST_INPROCESS=true` for the
  project build wrapper) drives the in-process collapse through the
  Dockerfile.

### What changes

- The server and the worker handler run in one address space; there is no
  child process and no plugin handshake.
- The pool is force-collapsed to a single worker. `--pool-size > 1` logs a
  warning and has no effect — multiple in-process handlers would share the
  same FFI surface, defeating any isolation a larger pool implies.
- Round-robin state still uses the same `SharedStateStore`, but the worker
  reads it through an in-process hook instead of dialling the host's
  shared-state gRPC broker.
- Pool-side panic recovery (`pool/recover_inprocess.go`) converts Go panics
  at every worker method boundary into structured `internal_error` responses
  with the original stack attached, so a recoverable panic surfaces as a
  normal error envelope rather than crashing the server.

### Surrendered guarantees

The supervision boundary is gone, so several behaviours that the
default build inherits from process isolation no longer apply:

- Rust/native aborts, segfaults, fatal runtime throws, and OOMs terminate
  the whole server. The orchestrator must restart it.
- Go panics on the direct worker method boundary are caught and returned as
  errors, but panics inside unrelated goroutines spawned by BAML or generated
  code can still crash the server.
- Native memory growth, goroutine leaks, OS-thread leaks, and FD leaks
  accumulate in the server process.
- Hung native calls cannot be killed independently of the request; a request
  timeout returns to the caller while the stuck work persists until the
  server is restarted.
- Per-worker `GOMEMLIMIT` and RSS monitoring no longer isolate a child.
- `/_debug/kill-worker` cannot terminate the server-side worker; the debug
  endpoint is degraded. `/_debug/native-stacks` reports
  `native worker stacks are unavailable in in-process builds`.

### When to use it

Memory-constrained deployments where running two Go runtimes (server +
worker) is the dominant overhead, the workload tolerates whole-process
restarts on FFI failure, and an orchestrator (Kubernetes, systemd, etc.)
already restarts the container on crash.

### When NOT to use it

Deployments that rely on BAML FFI panic/crash isolation, that need a
worker to die and be replaced without dropping unrelated in-flight
requests, or that drive `/_debug/kill-worker` and `/_debug/native-stacks`
as part of their operational toolkit.

### Operator requirements

A container restart policy, a memory limit sized for the merged process,
liveness/readiness probes that actually exercise the request path, and
tolerance for losing in-flight requests when the process crashes.

## Known bugs/limitations

### Upstream

- [ ] Compiler error if prompt inputs collide with imported package names in the
      generated BAML client
      ([issue](https://github.com/BoundaryML/baml/issues/2393))
- [x] Dynamic classes that do not include any static fields aren't properly
      handled ([issue](https://github.com/BoundaryML/baml/issues/2432))
- [ ] BAML runtime leaks goroutines
      ([workaround](https://github.com/invakid404/baml-rest/blob/master/cmd/hacks/hacks/context_fix.go))
      ([issue](https://github.com/BoundaryML/baml/issues/2883))
- [x] LLM client serialization fails if options contain a nested map
      ([workaround](https://github.com/invakid404/baml-rest/commit/dead72721909a9b9ef47b0ffd025e58615ec23eb)
      applied) ([issue](https://github.com/BoundaryML/baml/issues/2767); fixed
      in >=0.215.0)
- [ ] Streaming API doesn't propagate dynamically created classes to the parser.
      When creating a new class via TypeBuilder and referencing it from an
      existing `@@dynamic` class, the streaming API fails with "Class X not
      found". The sync API and Parse API work correctly. (reported to BAML team)

### baml-rest

- [ ] Optional (nullable) media types (`image?`, `(image | null)[]`, etc.) are
      not supported on BAML versions prior to 0.215.0. The BAML runtime's
      encoder rejects `*Image` with `unsupported type for BAML encoding`.
      Non-optional media types and nil optional values work on all versions.

## Known broken versions

Recommended version: **v0.223.0**

The BuildRequest code path (`BAML_REST_USE_BUILD_REQUEST=true`) requires
BAML **v0.219.0** or newer. Older BAML versions remain functional via the
CallStream+OnTick path, but baml-rest logs a warning at startup when no
BuildRequest API is detected in the generated client.

- **v0.215.0**: Type builder is fully broken and panics the entire application
  when used ([issue](https://github.com/BoundaryML/baml/issues/2862))
- **≥v0.216.0 <0.218.0**: Various issues in dynamic field handling (e.g.,
  [issue](https://github.com/BoundaryML/baml/issues/2966))
