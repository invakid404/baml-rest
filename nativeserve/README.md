# nativeserve — native transport for dynclient (de-BAML #624)

`github.com/invakid404/baml-rest/nativeserve` is the **public, `go get`-able,
nanollm-linked** native SERVE implementation for
[baml-rest](https://github.com/invakid404/baml-rest)'s in-process client library,
`dynclient`. It gives an in-process dynclient caller **native transport at parity
with the subprocess serve worker**: an admitted unary OpenAI `/call/_dynamic` call
is served natively (one exact provider RoundTrip, native translate/extract/parse,
the S4 plan-compare precondition and the S5 same-response BAML-parse safety
compare), with the **same tri-state disposition/error envelopes** and the **same
single umbrella-flag semantics** as the serve path.

It reuses the **exact same Slice-6 serve core** the subprocess worker links — no
divergent implementation.

## Why this doesn't change baml-rest's host invariant

baml-rest's own serving host (`cmd/serve`) and the **default** `dynclient` module
stay **zero-nanollm / CGO-free**. A third-party consumer that imports
`nativeserve` owns a **different** link graph — linking nanollm (CGO + the prebuilt
FFI archive) is *their* opt-in, not a change to baml-rest's host. Import this
module only when you want native transport in-process.

## Public API

```go
import (
    "github.com/prometheus/client_golang/prometheus"

    "github.com/invakid404/baml-rest/dynclient"
    "github.com/invakid404/baml-rest/nativeserve"
)

func newClient(reg prometheus.Registerer) (*dynclient.Client, error) {
    serve, err := nativeserve.New(reg) // bamlutils.NativeServeFunc
    if err != nil {
        return nil, err
    }
    return dynclient.New(
        dynclient.WithDeBAML(true),                       // umbrella flag; off = BAML transport
        dynclient.WithNativeServeComparator(serve),       // native transport (this module)
        // Render/parse are supplied separately via the pre-existing dynclient
        // seams (see the note below):
        //   dynclient.WithDeBAMLRenderer(myRenderer),    // bamlutils.DeBAMLRenderFunc
        //   dynclient.WithDeBAMLParser(myParser),        // bamlutils.DeBAMLParseFunc
        // ... plus client mode, HTTP client, client registry, etc.
    )
}
```

`nativeserve.New(reg)` returns the neutral `bamlutils.NativeServeFunc`. Pass it to
`dynclient.WithNativeServeComparator`. With `WithDeBAML(false)` (or the flag off),
the dynamic path stays byte-identical BAML with **zero** native FFI / socket / plan
build.

> **Renderer / parser.** This module owns only native **transport** (the serve
> func). The native `ctx.output_format` **renderer** and response **parser** are
> supplied separately through the existing `dynclient.WithDeBAMLRenderer`
> (`bamlutils.DeBAMLRenderFunc`) and `dynclient.WithDeBAMLParser`
> (`bamlutils.DeBAMLParseFunc`) seams. baml-rest's in-tree implementations live in
> the root module's `internal/debaml` (`Render` / `Parse`) and are therefore usable
> only by callers **inside** the baml-rest module; a third-party consumer supplies
> its own function of each type. (A public render/parse entry point is out of scope
> for #624, which brings dynclient's native **transport** to parity with the serve
> worker.)

### First-surface limits (inherited from admission)

Only admitted **unary OpenAI** `/call/_dynamic` calls are served natively.
Streaming, non-openai providers, fallback / round-robin chains, static clients, and
`call-with-raw` **decline pre-socket to BAML** — the same first surface as the
subprocess serve worker.

## CGO build recipe

This module links `github.com/viktordanov/nanollm-ffi/go` (v0.4.3), a **CGO**
package that links a prebuilt Rust static archive committed per-platform inside
that module. Building a consumer that imports `nativeserve` therefore requires:

- **`CGO_ENABLED=1`** and a working C toolchain (clang on macOS, gcc/clang on
  Linux).
- A **supported prebuilt OS/arch** for nanollm-ffi v0.4.3 (the archives shipped
  inside `nanollm-ffi/go`):
  - `darwin/arm64` (`aarch64-apple-darwin`)
  - `linux/amd64` (`x86_64-unknown-linux-gnu`)
  - `linux/arm64` (`aarch64-unknown-linux-gnu`)

  Other targets require a nanollm-ffi build for that platform.
- This module kept **out of any Go workspace** whose cold `go build ./...` must
  stay CGO-free. Resolve it as an ordinary dependency:

```bash
go get github.com/invakid404/baml-rest/nativeserve@latest
CGO_ENABLED=1 go build ./...
```

The pinned toolchain versions are: **nanollm-ffi `v0.4.3`**, **go-mocklm `v0.4.0`**
(test-only tool), **BAML `v0.223.0`**.

## Relationship to the subprocess worker

The subprocess serve worker
(`internal/nativebody/nanollmprepare/cmd/worker`) injects the same
`nativeserve.New` via `workerboot.Options.NativeServeFactory`, so the in-process
dynclient path and the subprocess serve path are at transport parity **by
construction** — one shared core, no duplication.
