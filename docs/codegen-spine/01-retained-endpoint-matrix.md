# Retained endpoint / method / type / client / option matrix (D2 ⚑)

> **⚑ Flagged for explicit owner sign-off.** This matrix *is* the frozen public
> contract the native lane must reproduce. Internal Go layout may change freely;
> the names, paths, JSON shapes, and semantics below may not, except by an
> explicit contract-version decision. Machine-checked subset:
> `internal/codegenspine/manifest.json` (`retained_endpoints`, `options_envelope`).

All grounding is at base master `d1f2526e2e7c`.

## 1. Endpoints

Five retained families, each served **per method** as `/<family>/<methodName>`
and (when a dynamic method is present) as `/<family>/_dynamic`. The dynamic
segment is `bamlutils.DynamicEndpointName = "_dynamic"` and the internal method
is `bamlutils.DynamicMethodName = "Baml_Rest_Dynamic"`
(`bamlutils/dynamic.go:86,88`).

| Endpoint | StreamMode | NeedsRaw | NeedsPartials | Success body | Servers | Route site |
|---|---|:--:|:--:|---|---|---|
| `/call/<m>` | `StreamModeCall` | no | no | structured output bytes, written directly | fiber, chi | `cmd/serve/main.go:478`, `cmd/serve/unary.go:92` |
| `/call-with-raw/<m>` | `StreamModeCallWithRaw` | yes | no | `CallWithRawResponse{data,raw,reasoning?}` | fiber, chi | `main.go:479`, `unary.go:93` |
| `/stream/<m>` | `StreamModeStream` | no | yes | NDJSON / SSE frame stream | fiber only | `main.go:494` |
| `/stream-with-raw/<m>` | `StreamModeStreamWithRaw` | yes | yes | NDJSON / SSE frames incl. `raw`+`reasoning` | fiber only | `main.go:495` |
| `/parse/<m>` | — (`ParseMethod`) | no | no | structured output bytes, written directly | fiber, chi | `main.go:498`, `unary.go:94` |

Two HTTP servers register these (both dispatch into the same `*pool.Pool`):

- **Fiber** (`--port`, default 8080): all five families incl. streaming.
- **Chi / net-http** (`--unary-port`, `0` = disabled): unary only — `/call`,
  `/call-with-raw`, `/parse`. **No** `/stream*` (streaming is fiber-only;
  `cmd/serve/unary.go:23`). This asymmetry is contract: a client cannot assume
  streaming on the unary port.

`NeedsRaw`/`NeedsPartials` are not free-floating — they are the live
`bamlutils.StreamMode` predicates (`bamlutils/interfaces.go:206-213`) and the
manifest is asserted against them. The single generated closure serves all four
call/stream modes via a bound `StreamMode` argument
(`makeCallHandler`/`makeStreamHandler`, `handler.CallStream` → `SetStreamMode`,
`worker/handler.go:528,553,564`).

## 2. Request envelopes

### 2.1 Static per-method call/stream

The request body decodes **twice** in `Handler.CallStream`
(`worker/handler.go:528`): once into the generated per-method input struct via
`method.MakeInput()` + `sonic.Unmarshal` (top-level fields are the method
arguments; unknown fields like `__baml_options__` are ignored), and once into
`workerBamlOptions` to extract `__baml_options__` (`handler.go:535-547`). **The
method name is the URL path segment; the arguments are the top-level JSON
object fields.**

### 2.2 Options object — `__baml_options__` (frozen)

Wire key `__baml_options__` → `bamlutils.BamlOptions`
(`worker/options.go:13`, `bamlutils/interfaces.go:1261`). Frozen field set:

| JSON | Go | Type | Meaning |
|---|---|---|---|
| `client_registry` | `ClientRegistry` | `*ClientRegistry` | client selection + `primary` |
| `type_builder` | `TypeBuilder` | `*TypeBuilder` | `{baml_snippets, dynamic_types}` overlay |
| `retry` | `Retry` | `*RetryConfig` | per-request retry override |
| `include_reasoning` | `IncludeReasoning` | `bool` | surface `reasoning` on `-with-raw` |
| `output_schema` | `OutputSchema` | `*DynamicOutputSchema` | dynamic output schema (de-BAML seams) |

Applied by `workerBamlOptions.apply` (`worker/options.go:23`) in a fixed order:
client registry → URL rewrites (keyed on `"base_url"`) → deployment client
defaults → trusted-config seal (last) → TypeBuilder → retry → include-reasoning →
de-BAML output schema. **HTTP client** is *not* per-request; it is per-handler
(`Config.HTTPClient`, installed via `SetHTTPClient`, `handler.go:462`).

### 2.3 `ClientRegistry` (frozen)

`bamlutils.ClientRegistry` (`interfaces.go:240`) and `ClientProperty`
(`interfaces.go:314`):

```go
type ClientRegistry struct {
    Primary *string           `json:"primary"`
    Clients []*ClientProperty `json:"clients"`
}
type ClientProperty struct {
    Name        string         `json:"name"`
    Provider    string         `json:"provider,omitempty"`
    RetryPolicy *string        `json:"retry_policy"`
    Options     map[string]any `json:"options,omitempty"`
}
```

**First-match-wins** semantics on the ordered `Clients` slice, with `Validate()`
rejecting duplicate/empty names (`ErrDuplicateClientName`, `ErrEmptyClientName`,
`interfaces.go:258,271`) — a contract the native lane must preserve exactly (BAML
upstream is last-wins-in-a-map; baml-rest deliberately is not).

### 2.4 Dynamic call/stream — `bamlutils.DynamicInput` (`dynamic.go:591`)

`messages` (`[]DynamicMessage`), `client_registry`, `output_schema`,
`preserve_schema_order,omitempty` (tri-state `*bool`), `include_reasoning,omitempty`.
`Validate` requires non-empty `messages`, a `client_registry` with `primary`,
and an `output_schema` with ≥1 property (`dynamic.go:619`). A `DynamicMessage`
is `{role, metadata?, content}` where `content` is a union (text string **or**
`[]DynamicContentPart`) decoded by custom `UnmarshalJSON` (`dynamic.go:215-229`).

### 2.5 Parse — `worker/parse.go:14` and dynamic `dynamic.go:757`

Static: `workerParseInput{ raw (required), stream,omitempty,
__baml_options__,omitempty }`. Empty `raw` → `"missing required field 'raw'"`
(`parse.go:40`). Dynamic `/parse/_dynamic`: `DynamicParseInput{ raw,
output_schema, preserve_schema_order?, stream? }` (`dynamic.go:757`). Parse-final
vs parse-stream is selected by `input.stream` against `ParseMethod.StreamImpl`; a
`stream` request against a nil `StreamImpl` errors `"parse method %q does not
support stream parse"` (`parse.go:73-79`).

## 3. Success output envelopes

- **`/call`, `/parse` (and `/stream` final, non-raw):** the structured output
  **bytes are written directly** — `result.Data` via `c.Send(...)` / `w.Write(...)`
  (`main.go:474,509`, `unary_handlers.go:101,124`). There is **no wrapper
  object**. Dynamic variants first `FlattenDynamicOutput` + `InjectAbsentOptionals`
  + reorder (`main.go:580`).
- **`-with-raw` unary:** `CallWithRawResponse{ data json.RawMessage, raw string,
  reasoning string,omitempty }` (`cmd/serve/main.go:856`). Sourced from
  `workerplugin.CallResult{Data, Raw, Reasoning}` (`plugin.go:57`); `raw` is
  "text-only by construction", `reasoning` empty unless `include_reasoning`.
- **`/stream*`:** content-negotiated `application/x-ndjson` vs `text/event-stream`
  (else `406`/`not_acceptable`). NDJSON frame is `NDJSONEvent{ type, data?, raw?,
  reasoning?, error?, code?, details? }` (`streamwriter.go:53`); `type` ∈
  `{data, final, reset, error, heartbeat, metadata}` (`streamwriter.go:35`). SSE
  event names: `final`, `reset`, `error`, `metadata`, and default (`message`) for
  partial data (`streamwriter.go:315,474`); with-raw SSE payloads carry a
  marshaled `CallWithRawResponse` (`streamwriter.go:459`).

## 4. What is public (frozen) vs internal (free to change)

**Frozen (contract):** the five endpoint paths and the `_dynamic` segment; the
per-method routing (method name = path segment); the request field names in
§2; `__baml_options__` and its field set; `CallWithRawResponse` field names;
the NDJSON/SSE frame `type`/field vocabulary; the content-negotiation types; the
first-match client-registry semantics; the error envelope (see
[03-public-compatibility-list.md](03-public-compatibility-list.md)).

**Internal (may change):** the generated Go input/output struct **layout**
(only its JSON projection is contract); which server (fiber vs chi) implements a
route, so long as the retained routes exist where documented; the worker
subprocess/protocol/pool wiring; the adapter object graph; whether serving is
generated-BAML or native.

**Observability headers** (`X-BAML-*`) are a separate class. `X-Request-Id` and
retry/winner metadata (`X-BAML-Retry-Count`, `X-BAML-Client`,
`X-BAML-Winner-Client`, …) are durable; the transition-diagnostic headers
`X-BAML-Path` / `X-BAML-Path-Reason` exist to observe native-vs-generated
routing during migration and are **not** frozen as permanent contract. They are
listed here so M6+ does not accidentally treat them as immutable.

## 5. Worker runtime seam (frozen; already sufficient)

`worker.Runtime` (`worker/runtime_iface.go:24`) is four methods and needs **no**
change for the native lane:

```go
type Runtime interface {
    InitRuntime()
    Method(name string) (bamlutils.StreamingMethod, bool)
    ParseMethod(name string) (bamlutils.ParseMethod, bool)
    MakeAdapter(ctx context.Context) bamlutils.Adapter
}
```

The `(value, ok)` lookups preserve the handler's `"method %q not found"` /
`"parse method %q not found"` contracts verbatim. `StreamingMethod{MakeInput,
MakeOutput, MakeStreamOutput, Impl}` and `ParseMethod{MakeOutput, Impl,
StreamImpl}` (`interfaces.go:217,226`) are the generation targets: the native
codegen backend emits these directly from descriptors, and a `NativeRuntime`
(M4) provides them behind this same interface. **D2 owner note:** keep
`worker.Runtime` frozen — adding to it would be a public seam change; the native
lane must fit within it.
