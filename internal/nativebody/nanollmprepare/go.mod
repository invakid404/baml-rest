// This is a SEPARATE, TEST-ONLY Go module for de-BAML's gated nanollm suites:
// Phase 4b's no-send OpenAI Prepare validation (root package + the static/dynamic
// P5 prepared-request differential) AND Slice 2's functional-send suite (the
// sibling execute package, which drives nanollm's real Do/DoStream over loopback
// against a go-mocklm subprocess). It exists solely to keep the PUBLIC
// github.com/viktordanov/nanollm-ffi/go dependency — and the go-mocklm test tool
// pinned below — OUT of the root module's go.mod/go.sum and out of the workspace
// module graph. nanollm-ffi is public and proxy/sumdb-resolvable, but it is still
// a CGO package that links a substantial prebuilt Rust static archive committed
// per-platform inside the module itself, so isolating it keeps a cold
// `go build ./...` / `go test ./...` (no tags) from ever needing nanollm, cgo, or
// a C toolchain. It is deliberately NOT a go.work member (see ../../../go.work),
// mirroring how dynclient/baml-patched and the adapters are excluded and resolved
// via replace directives. go-mocklm is pinned as a Go tool (the `tool` directive
// below) so it never enters the root/default build graph and is launched only as
// a subprocess by the gated execute suite / CI.
module github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare

go 1.26.5

require (
	github.com/boundaryml/baml v0.223.0
	github.com/invakid404/baml-rest v0.0.48
	github.com/invakid404/baml-rest/bamlutils v0.0.49-0.20260719190743-f08d06c95b1c
	github.com/invakid404/baml-rest/dynclient v0.0.0-00010101000000-000000000000
	github.com/invakid404/baml-rest/worker v0.0.49-0.20260719190743-f08d06c95b1c
	github.com/invakid404/baml-rest/workerplugin v0.0.48
	github.com/prometheus/client_golang v1.23.2
	github.com/prometheus/client_model v0.6.2
	github.com/viktordanov/nanollm-ffi/go v0.4.3
	golang.org/x/mod v0.37.0
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
	github.com/dave/jennifer v1.7.1 // indirect
	github.com/enriquebris/goconcurrentqueue v0.7.0 // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gregwebs/go-recovery v0.4.1 // indirect
	github.com/gregwebs/stackfmt v0.1.1 // indirect
	github.com/hashicorp/go-hclog v1.6.3 // indirect
	github.com/hashicorp/go-plugin v1.8.0 // indirect
	github.com/hashicorp/yamux v0.1.2 // indirect
	github.com/invakid404/baml-rest/adapters/adapter_v0_204_0 v0.0.0-00010101000000-000000000000 // indirect
	github.com/invakid404/baml-rest/adapters/adapter_v0_215_0 v0.0.0-00010101000000-000000000000 // indirect
	github.com/invakid404/baml-rest/adapters/adapter_v0_219_0 v0.0.0-00010101000000-000000000000 // indirect
	github.com/invakid404/baml-rest/adapters/common v0.0.48 // indirect
	github.com/invakid404/baml-rest/dynclient/baml-patched v0.0.48 // indirect
	github.com/invakid404/baml-rest/introspected v0.0.48 // indirect
	github.com/invakid404/baml-rest/nativeserve v0.0.0-00010101000000-000000000000
	github.com/invakid404/baml-rest/pool v0.0.0-00010101000000-000000000000 // indirect
	github.com/klauspost/compress v1.19.0 // indirect
	github.com/klauspost/cpuid/v2 v2.2.9 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/mitsuhiko/minijinja/minijinja-go/v2 v2.16.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/oklog/run v1.1.0 // indirect
	github.com/prometheus/common v0.69.0 // indirect
	github.com/prometheus/procfs v0.20.1 // indirect
	github.com/rs/zerolog v1.35.1 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasthttp v1.72.0 // indirect
	github.com/viktordanov/go-mocklm v0.4.0 // indirect
	golang.org/x/arch v0.0.0-20210923205945-b76863e36670 // indirect
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
	github.com/invakid404/baml-rest => ../../../
	github.com/invakid404/baml-rest/adapters/adapter_v0_204_0 => ../../../adapters/adapter_v0_204_0
	github.com/invakid404/baml-rest/adapters/adapter_v0_215_0 => ../../../adapters/adapter_v0_215_0
	github.com/invakid404/baml-rest/adapters/adapter_v0_219_0 => ../../../adapters/adapter_v0_219_0
	github.com/invakid404/baml-rest/adapters/common => ../../../adapters/common
	github.com/invakid404/baml-rest/bamlutils => ../../../bamlutils
	github.com/invakid404/baml-rest/bamlutils/sse => ../../../bamlutils/sse
	github.com/invakid404/baml-rest/dynclient => ../../../dynclient
	// The dynamic prepared-request leg links baml-rest's PATCHED BAML via
	// dynclient; a main-module replace is required because dynclient's own
	// `replace baml-patched => ./baml-patched` does not propagate across the
	// module boundary (this module is the main module under GOWORK=off).
	github.com/invakid404/baml-rest/dynclient/baml-patched => ../../../dynclient/baml-patched
	github.com/invakid404/baml-rest/introspected => ../../../introspected
	github.com/invakid404/baml-rest/pool => ../../../pool
	github.com/invakid404/baml-rest/worker => ../../../worker
	github.com/invakid404/baml-rest/workerplugin => ../../../workerplugin
)

tool github.com/viktordanov/go-mocklm

replace github.com/invakid404/baml-rest/nativeserve => ../../../nativeserve
