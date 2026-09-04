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
// are pinned to the ORIGIN-RESOLVABLE pseudo-version of a single commit — the one
// nativeserve/pin_followup.md records and cmd/build's TestFirstPartyPinFollowupIsTracked
// checks, which is CURRENTLY the M3e-A BRANCH SOURCE commit 3fa2816336c2 on
// feat/debaml-m3e-a (a BRANCH-ONLY pin whose tracked follow-up is OUTSTANDING — the
// post-squash re-pin to the M3e-A master squash commit is OWED; see the PIN-STATUS block
// below, pin_followup.md, and the M3e-A note by the pins). It SUPERSEDES the prior
// ExecBridge-U1c baseline 56d5473a1bdb, which was master-durable before M3e-A. It is NOT the
// historical Slice 7.1b master merge 15b98cf6ebe4 named in the floor notes below — one
// consistent snapshot that resolves cleanly off a fresh checkout with zero
// local replacements. The replace directives below
// are for local development only; Go
// ignores a dependency module's replaces, so an external consumer resolves those
// pseudo-versions itself, DIRECT from the origin repo under GOPRIVATE.
// When the root module is eventually tagged for a release that includes these
// packages, the three requires can be bumped to that tag.
//
// NOTE (Phase 7D): nativeserve/execute uses bamlutils' 7A exact-stream client,
// nativeserve/admission uses the root's nativebody.BuildOpenAIChatStream (Phase 7B),
// nativeserve/admission also imports the root's internal/debaml.SupportsNativeStream
// (Phase 7C — the native-stream SAP schema preflight, the §5.5 schema admission row),
// and nativeserve/canary + nativeserve.NewStream now return the neutral
// bamlutils.NativeStreamServeFunc (Phase 7D — guarded production wiring of the native
// stream lane, the new bamlutils/native_stream.go serve seam) — symbols that first
// appear at the 7A, 7B, 7C, and 7D merges respectively. The 7D merge (f08d06c95b1c)
// is therefore the FLOOR for the three first-party requires: any pin at or after it
// carries every one of those symbols.
//
// NOTE (de-BAML Slice 7.1b): nativeserve/admission + canary now carry the PROJECTED
// argument vector. admission.StaticInput.Values / StaticStreamInput.Values are
// []promptdescriptor.ArgumentValue (a NEW bamlutils type), they are copied from the
// equally new bamlutils.NativeStaticInvocation.Values / NativeStreamInvocation.Values,
// and they are what the root's internal/nativeprompt.SupportsStatic + RenderStatic now
// take in place of the raw Args map. Those symbols reach master in the Slice 7.1b
// squash-merge (15b98cf6ebe4, #655), which is why the three requires moved off the
// 2026-07-27 master snapshot (e7480ef6f2ef): against it, an external consumer failed
// to build with `undefined: promptdescriptor.ArgumentValue`.
//
// BUMP RULE. A bump must land on a commit whose root + bamlutils carry every symbol
// nativeserve links, and it must be a MASTER commit: only that survives branch deletion
// and is what an external consumer can resolve.
//
// WHETHER THE CURRENT PINS SATISFY THAT IS TRACKED, not left to this comment:
// nativeserve/pin_followup.md records the commit they name, whether it is master-durable
// yet, and — while it is not — the exact follow-up. cmd/build's
// TestFirstPartyPinFollowupIsTracked parses both that record and these requires on every
// ordinary `go test ./...`, and requires them to agree: all five selections must name ONE
// commit, the record must name that same commit and enumerate all five, and wherever a
// master ref is resolvable the record's STATUS must match actual master-reachability. So
// a branch-only pin is a written-down, machine-checked state rather than something a
// reader has to notice. Slice 7.1b briefly pinned its own
// PRE-MERGE branch commit (cff53b244ffa) because the symbols were introduced by that
// very change and no master commit carried them yet — then #655's squash-merge flattened
// that SHA out of master, its branch was deleted, and nativeserve-goget went red until
// the pins were moved here. Pin to a branch commit only if unavoidable, and re-pin to the
// merged master commit as the immediate follow-up.
//
// PIN-STATUS: OUTSTANDING
//
// That marker is the MACHINE-READABLE statement of where these pins stand, and
// cmd/build's TestPackagedManifestsMatchTheTrackedPins requires it to equal the
// STATUS in nativeserve/pin_followup.md — inside the packaged tar as well as
// here. It exists because prose is not auditable: a reviewer grepping this file
// for `f35489fc2463` or `OUTSTANDING` hits the HISTORICAL sentences below and can
// reasonably conclude the pins are elsewhere than they are. Read the marker, not
// the paragraphs.
//
// The COMPLETE post-squash re-pin runbook — every ordered step, including the two
// that are easiest to leave implicit (flipping BOTH mirrored manifest narratives,
// and materializing the external probe's module + main package before its
// go get/build/run) — is nativeserve/pin_followup.md's post-squash re-pin runbook
// section. Follow it there; do not reconstruct it from these paragraphs.
//
// NOTE (M3e-A — spine STREAM substrate): these pins are BRANCH-ONLY and the tracked
// follow-up in nativeserve/pin_followup.md is therefore STATUS: OUTSTANDING. They name
// 3fa2816336c2 on feat/debaml-m3e-a, the branch SOURCE commit that carries the M3e-A
// guarded-tree change, because no master commit carries it yet. This module gains the
// BAML-free spine STREAM lane: nativeserve/spine's StreamExecutor (embedding the frozen
// UnaryExecutor) plus StreamRegistration and the stream-native NewWorkerRuntime, and
// nativeserve/admission's AdmitStaticSpineStreamClaim with its unexported lane policy.
// The M3e-A nativeserve source depends on NEW bamlutils symbols — the neutral
// NativeSpineStreamExecutor / NativeSpineStreamBinding / NativeSpineStreamResult contract
// and the extracted BAML-free buildrequest.StreamCadence — so a consumer resolving a
// PRE-M3e-A bamlutils fails to compile this module; the lockstep is a build precondition,
// not cosmetic. Re-pinning all five to the MASTER squash commit immediately after the PR
// merges is the MANDATORY follow-up (see pin_followup.md's post-squash re-pin runbook).
//
// HISTORICAL, SUPERSEDED — the SHAs in this paragraph are not the current pins:
// Immediately before M3e-A, all five were MASTER-DURABLE at 56d5473a1bdb (STATUS: RESOLVED),
// the MASTER squash-merge commit of PR #713 that carried the ExecBridge-U1c live-oracle
// standard composite; U1c's pins were briefly BRANCH-ONLY at ae3900c1a0ff until #713
// squash-merged and its post-squash re-pin moved them to 56d5473a1bdb.
// Immediately before U1c, all five were MASTER-DURABLE at 8fe27577082c (STATUS: RESOLVED), the
// U1b MASTER squash-merge commit (PR #711) that carried the native-only packaged worker
// (nativeserve/spine.NewWorkerRuntime factory + population classifier + promoted pure adapter);
// U1b's pins were briefly BRANCH-ONLY at 9a67ca3741a7 on feat/debaml-execbridge-u1b until #711
// squash-merged and its post-squash re-pin moved them to 8fe27577082c.
// Immediately before U1b, all five were MASTER-DURABLE at 7ddbb39fd3db (STATUS: RESOLVED), the
// MASTER squash-merge commit of PR #708 that carried the ExecBridge-U1 spine unary lane +
// the new bamlutils NativeSpineUnaryExecutor / NativeSpineUnaryBinding / tri-state contract; U1's
// pins were briefly BRANCH-ONLY at f35489fc2463 on feat/debaml-execbridge-u1 (STATUS: OUTSTANDING)
// until #708 squash-merged and #709 re-pinned to 7ddbb39fd3db. Immediately before U1, all five
// were MASTER-DURABLE at 7c7bed8291b6 (STATUS: RESOLVED),
// the master commit that squash-merged the de-BAML /parse UNION-RESIDUAL batch-2 slice
// (PR #703); #703 was itself briefly BRANCH-ONLY at 5fdd679c8784 on
// feat/debaml-parse-batch2 (re-bumped there twice — off the initial batch-2 source
// c011c7e95993, through a75a73df084e, as the Codex and CodeRabbit reviews added guarded-tree
// changes) for as long as no master commit carried the SAP. #703's squash flattened those
// SHAs out of history and the branch was deleted, orphaning the pins; repairing that is what
// this re-pin is. Immediately before batch 2, all five were MASTER-DURABLE at 062871154d95
// (STATUS: RESOLVED), the master tip after #689's union burn-down (squash cf03786a1fac) plus
// the M1 codegen spine (#691) that moved bamlutils on top of it; #692 performed that
// post-squash re-pin. #689's own pins were BRANCH-ONLY at 83dde65a20f1 on
// feat/debaml-parse-union before that squash flattened them out. Before it, the /parse
// burn-down batch 1 pins ended at 05102106c569, the #686 master squash-merge, after being
// briefly BRANCH-ONLY at 251b09219943 on feat/debaml-parse-burndown-1 (#687 performed that
// re-pin). Before that, S3b's pins were at ba813ad3564d (#683) after 4a6c8f1ee571, S3a's at
// 2f2e13c6dadb (#681) after 9f4cfe14e878, S2's at c676a4dacc90 (#677) after e38f7effd633,
// and S1's at de1eefa68ed8 (#675) after 7cb69af02b36. That branch-pin-then-re-pin sequence
// is the precedent this bump repeats.
//
// This bump carries real cross-module behaviour, not a version string. The M3e-A change adds
// new NEUTRAL contracts in bamlutils (NativeSpineStreamExecutor / NativeSpineStreamBinding /
// NativeSpineStreamEvent / NativeSpineStreamResult, and the extracted BAML-free
// buildrequest.StreamCadence) that this module's spine STREAM lane
// (nativeserve/spine.StreamExecutor + nativeserve/admission.AdmitStaticSpineStreamClaim) is
// built directly on top of. A consumer resolving a PRE-M3e-A pin gets a bamlutils that
// predates those contracts, so this module fails to compile against it — the lockstep is not
// cosmetic here, it is a build precondition. The emitted/runtime spine path itself remains free
// of generated BAML and BAML CFFI (proven by go list -deps over the emitted modules, the spine
// package, and the whole packaged native-only command); nanollm FFI is the intended native
// engine and is permitted.
//
// The precedent for this follow-up was #681 -> #682, #683 -> #684, #686 -> #687,
// #689 -> #692, #703's batch-2 re-pin, U1's #708 -> #709, U1b's #711 post-squash re-pin, and
// U1c's #713 post-squash re-pin: pin to the branch SOURCE commit only because no master commit
// carries the change yet, then re-pin all five to master and regenerate the tar IMMEDIATELY
// after the merge. This change is the BRANCH-pin half of that pattern for M3e-A: all five pins
// name the branch source commit 3fa2816336c2 (STATUS: OUTSTANDING) with the tar regenerated in
// the same change, and the post-squash master re-pin is OWED.
//
// A bump must ALSO move internal/nativebody/nanollmprepare/go.mod's recorded bamlutils +
// worker selections in LOCKSTEP: nanollmprepare directory-replaces root / bamlutils /
// worker / nativeserve, so only the version STRINGS reach MVS, and raising the selected
// version without recording it there fails the native-worker PACKAGING build
// (-mod=readonly) with "updates to go.mod needed". Note cmd/build/nativeworker_module.tar
// embeds both manifests, so a bump must regenerate it (go run ./cmd/build/gen-nativeworker-src).
// The nativeserve-goget gate sets GOPRIVATE=github.com/invakid404/baml-rest, so these
// pseudo-versions resolve DIRECT off the origin with no checksum-db lookup.
module github.com/invakid404/baml-rest/nativeserve

go 1.26.5

require (
	github.com/bytedance/sonic v1.15.2
	github.com/invakid404/baml-rest v0.0.0-20260904113537-3fa2816336c2
	github.com/invakid404/baml-rest/bamlutils v0.0.49-0.20260904113537-3fa2816336c2
	github.com/invakid404/baml-rest/worker v0.0.49-0.20260904113537-3fa2816336c2
	github.com/invakid404/baml-rest/workerplugin v0.0.48
	github.com/prometheus/client_golang v1.23.2
	github.com/prometheus/client_model v0.6.2
	github.com/viktordanov/nanollm-ffi/go v0.4.3
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
	github.com/bytedance/sonic/loader v0.5.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/cloudwego/gjson v0.1.1 // indirect
	github.com/dave/jennifer v1.7.1 // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/hashicorp/go-hclog v1.6.3 // indirect
	github.com/hashicorp/go-plugin v1.8.0 // indirect
	github.com/hashicorp/yamux v0.1.2 // indirect
	github.com/invakid404/baml-rest/adapters/common v0.0.48 // indirect
	github.com/invakid404/baml-rest/introspected v0.0.48 // indirect
	github.com/invakid404/minijinja-go/v2 v2.16.0-baml.6 // indirect
	github.com/klauspost/compress v1.19.0 // indirect
	github.com/klauspost/cpuid/v2 v2.2.9 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/oklog/run v1.1.0 // indirect
	github.com/prometheus/common v0.69.0 // indirect
	github.com/prometheus/procfs v0.20.1 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/stoewer/go-strcase v1.3.1 // indirect
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
	golang.org/x/text v0.39.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/grpc v1.82.1 // indirect
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
