// This is a SEPARATE, TEST-ONLY Go module for de-BAML Phase 4b's gated,
// no-send nanollm OpenAI Prepare validation. It exists solely to keep the
// PRIVATE github.com/viktordanov/nanollm/go dependency OUT of the root
// module's go.mod/go.sum and out of the workspace module graph, so a cold,
// credential-less `go build ./...` / `go test ./...` (no tags) never needs
// nanollm at all. It is deliberately NOT a go.work member (see ../../../go.work),
// mirroring how dynclient/baml-patched and the adapters are excluded and
// resolved via replace directives.
module github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare

go 1.26.5

require (
	github.com/invakid404/baml-rest v0.0.48
	github.com/viktordanov/nanollm/go v0.3.0
)

require (
	github.com/alecthomas/participle/v2 v2.1.4 // indirect
	github.com/andybalholm/brotli v1.2.2 // indirect
	github.com/aws/aws-sdk-go-v2 v1.42.0 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.13 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.32.25 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.24 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.29 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.29 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.29 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.30 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.29 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.2.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.31.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.36.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.43.3 // indirect
	github.com/aws/smithy-go v1.27.2 // indirect
	github.com/bytedance/gopkg v0.1.3 // indirect
	github.com/bytedance/sonic v1.15.2 // indirect
	github.com/bytedance/sonic/loader v0.5.1 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/cloudwego/gjson v0.1.1 // indirect
	github.com/invakid404/baml-rest/bamlutils v0.0.48 // indirect
	github.com/klauspost/compress v1.19.0 // indirect
	github.com/klauspost/cpuid/v2 v2.2.9 // indirect
	github.com/mitsuhiko/minijinja/minijinja-go/v2 v2.16.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasthttp v1.72.0 // indirect
	golang.org/x/arch v0.0.0-20210923205945-b76863e36670 // indirect
	golang.org/x/mod v0.37.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
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
	github.com/invakid404/baml-rest/introspected => ../../../introspected
	github.com/invakid404/baml-rest/pool => ../../../pool
	github.com/invakid404/baml-rest/worker => ../../../worker
	github.com/invakid404/baml-rest/workerplugin => ../../../workerplugin
)
