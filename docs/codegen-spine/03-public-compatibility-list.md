# Exact public compatibility list

The Go/JSON bytes and typed error metadata the native lane must preserve. **Parity
authority (D8):** the public HTTP/plugin JSON bytes and typed error metadata are
authoritative; generated Go struct layout and source text need compatibility only
where a downstream Go consumer is explicitly retained. Machine-checked subset:
`internal/codegenspine/manifest.json` (`error_taxonomy`, `options_envelope`) —
grounded against `internal/apierror`, `bamlutils.BamlOptions`, and
`bamlutils.StreamMode`.

## 1. Error envelope (frozen)

`internal/apierror.Response` (`internal/apierror/error.go:133`):

```go
type Response struct {
    Error     string             `json:"error"`
    Code      Code               `json:"code,omitempty"`
    Details   json.RawMessage    `json:"details,omitempty"`
    RequestID string             `json:"request_id,omitempty"`
}
```

`error` always present; `code`/`details`/`request_id` omitempty. `details`, when
present, is a **JSON object** (validated before emission, `error.go:158-188`).

## 2. Error code taxonomy (frozen — 11 codes, declaration order)

The canonical enum (`internal/apierror/error.go:20-32,84-125`; the manifest
freezes this list order-sensitively against `apierror.AllCodes()`):

| # | Code | Owner | Notes |
|---:|---|---|---|
| 1 | `invalid_json` | host | body not valid JSON |
| 2 | `invalid_request` | host | valid JSON, failed schema/semantic validation |
| 3 | `request_too_large` | host | body exceeded size limit |
| 4 | `body_read_error` | host | I/O error reading body |
| 5 | `not_acceptable` | host | `Accept` doesn't intersect producible formats |
| 6 | `request_canceled` | host | client disconnect / deadline |
| 7 | `worker_unavailable` | host | pool exhausted retries on retryable infra failure |
| 8 | `worker_error` | **worker** | residual worker failure (catch-all) |
| 9 | `provider_error` | **worker** | upstream non-2xx or transport failure |
| 10 | `parse_error` | **worker** | couldn't coerce/validate raw text into schema |
| 11 | `internal_error` | **worker** | Go-side processing bug / panic inside worker |

**Worker-facing subset (frozen):** `worker_error`, `provider_error`,
`parse_error`, `internal_error` (`Code.IsWorkerFacing()`, `error.go:76-82`). A
worker may self-classify **only** as one of these four; the host drops any
off-contract or non-worker-facing code and re-classifies
(`normalizeWorkerMetadata`, `cmd/serve/error.go:209`). The worker constants
`worker/errors.go:22-26` (`"parse_error"`, `"provider_error"`, `"internal_error"`)
must stay in sync with the public enum.

## 3. `provider_error` details (frozen field set)

`worker.providerErrorDetails` (`worker/errors.go:86-107`) — the object under
`details` for provider errors:

```go
status_code        int    // omitempty — HTTP status when upstream produced a response
body               string // omitempty — response body (developer tool: unredacted)
client_name        string // omitempty — from legacy LLM-client envelope parsing
error_code         string // omitempty — AWS/bedrock stream transport error code
error_message      string // omitempty
exception_type     string // omitempty — bedrock stream exception
exception_message  string // omitempty
```

Panic recovery emits `internal_error` with a `{"stacktrace": "..."}` detail
(`worker/stream_recover_inprocess.go:19`, `cmd/serve/error.go`
`stacktraceDetailsJSON`).

> **⚑ Security / privacy — flagged owner decision (before native cutover).**
> `provider_error.details.body` (the raw upstream response body) and the
> `internal_error` `stacktrace` detail are **unredacted by existing design**:
> baml-rest is positioned as a developer tool, so it surfaces upstream payloads
> and stack traces to aid debugging (see the explicit rationale at
> `worker/errors.go:78` and `internal/apierror/error.go:110-124`). A raw provider
> body can carry PII, secrets echoed by the provider, or internal paths, and a
> stack trace exposes internal file paths.
>
> **M0 only documents this pre-existing behavior; it introduces no new
> exposure** and deliberately makes **no runtime change** (redaction lives in the
> response-building paths — `worker/errors.go`, `stacktraceDetailsJSON`, stream
> recovery — which are out of a docs/ADR + manifest freeze slice). **Whether to
> redact `body`/`stacktrace` or gate them behind an authenticated developer-only
> mode is an owner decision that must be settled before native serving cuts over
> (M6+),** because freezing these as public contract now would otherwise lock the
> exposure in. Recommended default: keep them for the developer-tool posture but
> add a deployment flag to redact/gate for untrusted-facing deployments, with a
> regression test asserting the gated mode exposes neither — implemented in the
> milestone that owns the error path, not here. Tracked as an open item in
> [05-decisions-adr.md](05-decisions-adr.md).

### Typed error sources → code (frozen mapping)

The native lane must reproduce these classifications (`classifyBAMLError`,
`worker/errors.go:202-307`), in this precedence:

| Typed source | Code | Details |
|---|---|---|
| `buildrequest.ErrOutputParse` (sentinel) | `parse_error` | none |
| `*llmhttp.HTTPError{StatusCode, Body}` | `provider_error` | `status_code`, `body` |
| `llmhttp.ErrTransportFlake` (sentinel) | `provider_error` | none |
| `*awsstream.TransportError{Code, Message}` | `provider_error` | `error_code`, `error_message` |
| `*buildrequest.BedrockStreamException` | `provider_error` | `exception_type`, `exception_message` |

Reuse these typed sources; **do not** encode BAML display-prefix strings in new
native paths. Legacy first-line prefix parsing (`"Parsing error: "`,
`LLM client "..." failed with status code: ...`, `worker/errors.go:38-43,271-304`)
stays **oracle-only** and is deleted at rank 6/7 (scope §5 risk 6).

## 4. Descriptor / type contracts (frozen; reused, not reinvented)

The native carrier generator (M3) must preserve the JSON shape of these.

### `bamlutils.Checked[T]` (`bamlutils/checked.go:61`)

```go
type Checked[T any] struct {
    Value  T                `json:"value"`
    Checks map[string]Check `json:"checks"`
}
type Check struct {
    Name       string `json:"name"`
    Expression string `json:"expression"`
    Status     string `json:"status"`   // "succeeded" | "failed"
}
```

`MarshalJSON` emits `value` then `checks` in declaration order; a failed check
still emits its value (`checked.go:86,176`). This carrier appears **inside**
`data`, not as a top-level response field.

### `schemadescriptor` type vocabulary (`Version = 1`)

`Bundle{version, method?, stream?, target, enums, classes, recursive_classes,
structural_recursive_aliases}` (`descriptor.go:52`), all plural fields ordered
slices. `TypeKind` ∈ `{top, primitive, enum, literal, class, list, map,
recursive_alias, tuple, arrow, union}` (`descriptor.go:126-140`). `PrimitiveKind`
∈ `{string, int, float, bool, null, media}`. `MediaKind` ∈ `{image, audio, pdf,
video}` (`descriptor.go:179-186`). `LiteralKind` ∈ `{string, int, bool}`.
`StreamingMode` ∈ `{non_streaming, streaming}`. `StreamingBehavior{needed, done,
state}`. `ConstraintLevel` ∈ `{check, assert}` carried opaquely on
`TypeMeta`/`EnumDef`/`ClassDef` `Constraints`. `Name{name, alias?}` where
nil-vs-present alias is significant.

The lowering fence (`FromStaticDescriptor`, `internal/schema/static_descriptor.go:54`)
re-validates every enum string and structural invariant fail-closed: unknown
enum = error (never blind cast), missing required child = error, null primitive
as union variant = error, media without subtype = error. The native lane must
be at least this strict.

### `promptdescriptor.Function` (`Version = 3`)

`{Version, Method, Prompt (raw body), Args, Client, Provider, Return
(schemadescriptor.Bundle), Macros, ClientConfig (v2+), InputValues (v3+)}`
(`descriptor.go:244`). `ClientModel.Provenance ∈ {"", literal, env, dynamic}`;
`OptionValueKind ∈ {string, number, bool, ident, env, list, object}`;
`ClientConfig` splits `TransportOptions` vs `BodyAffectingOptions`
(`descriptor.go:394-403`). These distinctions are contract for the native client
planner.

## 5. `workerplugin` carriers (frozen shapes across the subprocess boundary)

- `ErrorWithStack{Err, Stacktrace, Code, Details}` (`workerplugin/plugin.go:77`,
  constructor `NewErrorWithMetadata` `:130`); over gRPC the fields map to
  `Stacktrace`, `ErrorCode`, `ErrorDetailsJson`, `Error` (`grpc.go`). The host
  lifts `Code`/`Details` onto `apierror.Response` only when trusted (§2).
- `CallResult{Data []byte, Raw string, Reasoning string, Planned, Outcome}`
  (`plugin.go:57`); `Raw` is text-only by construction, `Reasoning` empty unless
  `include_reasoning`.
- `StreamResult{Kind, Data, Raw, Reasoning, Error, Stacktrace, Reset, ErrorCode,
  ErrorDetails}` with `Kind ∈ {Stream, Final, Error, Heartbeat, Metadata}`
  (`plugin.go:138,158`).
- `ParseResult{Data []byte}` (`plugin.go:174`).

**Keep the worker subprocess/protocol/pool unchanged** (scope §1.2): these layers
deal in method name, JSON, stream results, parse results, lifecycle, and retries;
they do not need a BAML source/runtime model.
