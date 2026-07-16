// Module github.com/invakid404/baml-rest/nativeserve is the PUBLIC, go-get-able,
// nanollm-linked native SERVE implementation for de-BAML (#624). It hosts the
// reusable Slice-6 serve core — admission / execute / parity / canary plus the
// testutil / planassert helpers, relocated out of the internal, out-of-go.work
// internal/nativebody/nanollmprepare module so that BOTH the subprocess serve
// worker AND an in-process dynclient consumer link the SAME core with no
// duplication. Its public entry point is nativeserve.New (see nativeserve.go): a
// dynclient caller passes it to dynclient.WithNativeServeComparator to obtain
// native transport at parity with the subprocess serve path.
//
// It links github.com/viktordanov/nanollm-ffi/go (public, proxy/sumdb-resolvable)
// — a CGO package that links a substantial prebuilt Rust static archive committed
// per-platform inside that module — so it is deliberately NOT a go.work member
// (see ../go.work), mirroring how nanollmprepare / dynclient/baml-patched / the
// adapters are excluded and resolved via the replace directives below. Keeping it
// out of the workspace is what preserves the load-bearing invariant that a cold
// `go build ./...` / `go test ./...` at the root — and the DEFAULT dynclient
// module graph — never need nanollm, cgo, or a C toolchain: only a consumer who
// explicitly imports THIS module pulls nanollm in. See README.md / doc.go for the
// CGO build recipe. go-mocklm is pinned as a Go tool (the `tool` directive below)
// so it never enters a consumer's default build graph and is launched only as a
// subprocess by the gated execute suite / CI.
//
// go-get resolvability: this module requires ONLY the root module (for the
// zero-nanollm internal debaml/nativebody/nativeprompt/schema packages), its
// bamlutils + worker siblings, and public nanollm-ffi — never the replace-only
// nanollmprepare module. The root module carries no release TAG (only the
// submodules bamlutils/worker are tagged, and their tagged versions predate the
// packages the root's internal/* imports here), so the three first-party requires
// are pinned to the PROXY-RESOLVABLE pseudo-version of the master commit this
// module targets (d7359348) — one consistent snapshot that resolves cleanly off a
// fresh checkout with zero local replacements. The replace directives below are for
// local development only; Go ignores a dependency module's replaces, so an external
// consumer resolves those pseudo-versions from the proxy. When the root module is
// eventually tagged for a release that includes these packages, the three requires
// can be bumped to that tag.
module github.com/invakid404/baml-rest/nativeserve

go 1.26.5

require (
	github.com/invakid404/baml-rest v0.0.0-20260716063753-d7359348a2a5
	github.com/invakid404/baml-rest/bamlutils v0.0.49-0.20260716063753-d7359348a2a5
	github.com/invakid404/baml-rest/worker v0.0.49-0.20260716063753-d7359348a2a5
	github.com/prometheus/client_golang v1.23.2
	github.com/prometheus/client_model v0.6.2
	github.com/viktordanov/nanollm-ffi/go v0.3.2
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/alecthomas/participle/v2 v2.1.4 // indirect
	github.com/andybalholm/brotli v1.2.2 // indirect
	github.com/aws/aws-sdk-go-v2 v1.42.1 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.14 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.32.25 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.24 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.29 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.30 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.30 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.30 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.29 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.2.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.31.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.36.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.43.3 // indirect
	github.com/aws/smithy-go v1.27.3 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bytedance/gopkg v0.1.3 // indirect
	github.com/bytedance/sonic v1.15.2 // indirect
	github.com/bytedance/sonic/loader v0.5.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/cloudwego/gjson v0.1.1 // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/hashicorp/go-hclog v1.6.3 // indirect
	github.com/hashicorp/go-plugin v1.8.0 // indirect
	github.com/hashicorp/yamux v0.1.2 // indirect
	github.com/invakid404/baml-rest/workerplugin v0.0.48 // indirect
	github.com/klauspost/compress v1.19.0 // indirect
	github.com/klauspost/cpuid/v2 v2.2.9 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/mitsuhiko/minijinja/minijinja-go/v2 v2.16.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/oklog/run v1.1.0 // indirect
	github.com/prometheus/common v0.69.0 // indirect
	github.com/prometheus/procfs v0.20.1 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasthttp v1.72.0 // indirect
	github.com/viktordanov/go-mocklm v0.4.0 // indirect
	golang.org/x/arch v0.0.0-20210923205945-b76863e36670 // indirect
	golang.org/x/mod v0.37.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/grpc v1.82.0 // indirect
	google.golang.org/protobuf v1.36.12-0.20260120151049-f2248ac996af // indirect
)

// Resolve the root module and every locally-replaced sibling from the checkout
// (this module is outside go.work, so root's own replaces do not propagate).
replace (
	github.com/invakid404/baml-rest => ../
	github.com/invakid404/baml-rest/adapters/adapter_v0_204_0 => ../adapters/adapter_v0_204_0
	github.com/invakid404/baml-rest/adapters/adapter_v0_215_0 => ../adapters/adapter_v0_215_0
	github.com/invakid404/baml-rest/adapters/adapter_v0_219_0 => ../adapters/adapter_v0_219_0
	github.com/invakid404/baml-rest/adapters/common => ../adapters/common
	github.com/invakid404/baml-rest/bamlutils => ../bamlutils
	github.com/invakid404/baml-rest/bamlutils/sse => ../bamlutils/sse
	// nativeserve imports NEITHER dynclient nor baml-patched, but the root module
	// (which nativeserve requires) references both in its own module graph. Under
	// GOWORK=off with nativeserve as the main module (local dev + the nanollm-send
	// CI lane), root's own replaces do not propagate, and these first-party siblings
	// have no published version — so nativeserve carries local filesystem replaces
	// for them (as for every root-graph sibling above) to keep `go mod download` /
	// tidy resolving from the checkout instead of an unpublished pseudo-version.
	github.com/invakid404/baml-rest/dynclient => ../dynclient
	github.com/invakid404/baml-rest/dynclient/baml-patched => ../dynclient/baml-patched
	github.com/invakid404/baml-rest/introspected => ../introspected
	github.com/invakid404/baml-rest/pool => ../pool
	github.com/invakid404/baml-rest/worker => ../worker
	github.com/invakid404/baml-rest/workerplugin => ../workerplugin
)

tool github.com/viktordanov/go-mocklm
