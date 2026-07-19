package bamlunicode

// Immutable BAML 0.223.0 Unicode compatibility profile.
//
// This is a DUAL-VERSION profile. The stock BoundaryML BAML 0.223.0 CFFI build
// (rustc 1.93.0) resolves its observable match_string Unicode behavior against
// two INDEPENDENTLY versioned datasets, and reproducing BAML byte-for-byte means
// reproducing BOTH:
//
//   - Case + character properties (str::to_lowercase incl. SpecialCasing 1->many
//     and the language-independent Final_Sigma context, char::is_alphanumeric,
//     char::is_whitespace / str::trim, and the internal Cased / Case_Ignorable
//     properties Final_Sigma consults) come from Rust 1.93.0 std, whose
//     char::UNICODE_VERSION is 17.0.0.
//
//   - NFKD and Mn/Mc/Me combining-mark classification come from
//     unicode-normalization 0.1.24, whose UNICODE_VERSION is 16.0.0.
//
// This is why bumping Go or golang.org/x/text cannot close #555: x/text under a
// newer Go selects a single Unicode version for normalization, while BAML pins
// case=17 and normalization=16 simultaneously. The values below are the
// compatibility key; the CI drift guard re-derives every one of them from the
// pinned BAML source on each BAML bump and fails if any drifts (see
// .github/workflows and ./cmd/gen). They are NOT inferred from marketing
// version strings — RustStdUnicodeVersion is what rustc 1.93.0's
// char::UNICODE_VERSION probe prints, and NormalizationUnicodeVersion is what a
// locked unicode_normalization::UNICODE_VERSION probe prints.
const (
	// BAMLVersion is the stock BoundaryML BAML release this profile reproduces.
	BAMLVersion = "0.223.0"
	// BAMLSourceCommit is the pinned BAML source commit (integration/baml_versions.json).
	BAMLSourceCommit = "85247f452fe202300ec38d41fdc4035ea3020983"

	// RustcVersion is the toolchain BAML's libbaml_cffi is built with
	// (rust-toolchain.toml + the setup-rust action default at the pinned commit).
	RustcVersion = "1.93.0"
	// RustcCommit is the rustc 1.93.0 source commit.
	RustcCommit = "254b59607d4417e9dffbc307138ae5c86280fe4c"
	// RustStdUnicodeVersion is rustc 1.93.0 std char::UNICODE_VERSION: the dataset
	// backing str::to_lowercase and the std character properties.
	RustStdUnicodeVersion = "17.0.0"

	// NormalizationCrate / NormalizationCrateVersion identify the crate BAML's
	// engine/Cargo.lock resolves for NFKD and combining-mark classification.
	NormalizationCrate        = "unicode-normalization"
	NormalizationCrateVersion = "0.1.24"
	// NormalizationCrateChecksum is that crate's Cargo.lock checksum.
	NormalizationCrateChecksum = "5033c97c4262335cded6d6fc3e5c18ab755e1a3dc96376350f3d8e9f009ad956"
	// NormalizationUnicodeVersion is unicode_normalization::UNICODE_VERSION.
	NormalizationUnicodeVersion = "16.0.0"

	// MatchStringFingerprint is the SHA-256 of BAML's pinned match_string.rs.
	// The drift guard fails if the upstream matcher is rewritten even when both
	// Unicode versions are unchanged (an algorithm change the version numbers
	// cannot detect).
	MatchStringFingerprint = "6ced35949641470658930d60787d128f9ccf5bb9b058a2606b64c81f54d1d228"
	// MatchStringPath is the upstream matcher held under observation.
	MatchStringPath = "engine/baml-lib/jsonish/src/deserializer/coercer/match_string.rs"

	// ReleaseWorkflowPath / ReleaseWorkflowFingerprint pin the BAML release
	// workflow that builds libbaml_cffi (cargo build --release -p baml_cffi). It
	// is an authoritative source for the RELEASED toolchain: a change to how the
	// artifact is built must fail the guard even if rust-toolchain.toml is
	// untouched.
	ReleaseWorkflowPath        = ".github/workflows/build-cli-release.reusable.yaml"
	ReleaseWorkflowFingerprint = "1aa156be43a455693245aad57be996673f03384f8045589550837ab6ef6c1ad2"
	// SetupRustActionPath / SetupRustActionFingerprint pin the setup-rust action
	// the release workflow resolves; its `toolchain` input default is the
	// effective release toolchain and must equal RustcVersion.
	SetupRustActionPath        = ".github/actions/setup-rust/action.yml"
	SetupRustActionFingerprint = "31bdf1bf41065f8b1e429ffeef768b97d63ae7f5196be094c498057cadfcdfa3"
)
