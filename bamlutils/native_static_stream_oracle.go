package bamlutils

import (
	"context"
	"reflect"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// ExecBridge-U1s / M3e-B — the NEUTRAL standard-worker STATIC-STREAM ORACLE seam.
//
// It is the streaming twin of the unary [NativeStaticInvocation] oracle contract and the
// ORACLE twin of the legacy [NativeStaticStreamInvocation]. Where the legacy static-stream
// seam hands the transport to a native implementation and keeps partial/final PARSING in
// the orchestrator, this seam hands the WHOLE claimed stream — transport, per-prefix
// cadence, and the final — to one implementation which resolves every structured tick and
// the final against a LIVE BAML oracle over the SAME accumulated prefix.
//
// WHY THE CALLBACKS ARE NEUTRAL FUNCTION VALUES. The standard generated method is already
// linked against BAML, so it (and only it) can name `ParseStream.<Method>` / `Parse.<Method>`.
// It supplies them here as plain Go closures. Nothing on the receiving side —
// bamlutils, bamlutils/buildrequest, nativeserve/spine, the emitted native package, or the
// standard composite — imports github.com/boundaryml/baml or a generated baml_client. That
// is both lower coupling and a stronger isolation proof than a direct BAML import would be,
// and it is what keeps the native-only artifact's dependency gate meaningful.
//
// SENSITIVE: Descriptor embeds the function's raw static prompt bytes and any inline
// client-option literals (including literal credentials); BuildBAMLStreamRequest returns a
// plan carrying the real bearer Authorization + request body; every parsed Value is real
// provider output. Nothing here may be logged, serialized, %v-formatted, error-wrapped, or
// emitted into a metric — only the bounded disposition/stage/reason/winner tokens are safe.

// BAMLStreamPrefixResult is the outcome of ONE BAML `ParseStream.<Method>` over one
// accumulated prefix. The two states are explicit so a caller can never confuse "BAML has
// no partial for this prefix yet" with "BAML produced a typed nil".
//
// HasValue is AUTHORITATIVE: a nullable family may legitimately produce a present typed-nil
// partial, which must be compared and emitted rather than collapsed into a no-value.
type BAMLStreamPrefixResult struct {
	// Value is BAML's typed partial for this prefix when HasValue. SENSITIVE.
	Value any
	// HasValue reports that BAML established a partial for this prefix.
	HasValue bool
}

// BAMLStreamPrefixValue normalizes ONE successful generated `ParseStream.<Method>` return
// into the neutral prefix result. It exists so the "what counts as a value" rule is stated
// ONCE, next to the contract, rather than re-derived in every generated closure.
//
// A nil interface and a NIL POINTER both report HasValue=false. Generated ParseStream
// returns either a value type or a pointer depending on the method's stream carrier, and a
// nil pointer boxed in an `any` is a non-nil interface — so a plain `v != nil` check would
// read a nil carrier as a present partial and hand it to the comparison, where it marshals
// as `null`.
//
// For the exact `ClassStaticStream` cohort this is unobservable: its stream carrier is a
// non-nullable union value, so a nil success cannot arise. The rule is stated for the
// methods a later slice admits, and it is the SAFE direction there — a BAML no-value
// SUPPRESSES the tick rather than releasing an unverified partial.
func BAMLStreamPrefixValue(v any) BAMLStreamPrefixResult {
	if v == nil {
		return BAMLStreamPrefixResult{}
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Ptr && rv.IsNil() {
		return BAMLStreamPrefixResult{}
	}
	return BAMLStreamPrefixResult{Value: v, HasValue: true}
}

// BAMLStreamParse runs BAML's `ParseStream.<Method>` over ONE accumulated prefix and
// normalizes BAML's partial semantics into the closed contract below. It performs NO
// transport: no request builder or send function is reachable from it.
//
//   - a successful non-nil ParseStream result is (Value, HasValue=true, nil);
//   - an ORDINARY partial-parse rejection — the expected outcome for an incomplete prefix —
//     is (zero, nil): an AUTHORITATIVE "no partial yet", never an error;
//   - context cancellation/deadline, option-construction failure, a marshaling/invariant
//     failure, or any other outcome that is NOT an ordinary ParseStream rejection returns a
//     NON-NIL error, meaning the oracle result could not be ESTABLISHED. On a claimed
//     stream that is terminal.
//
// A PANIC is never converted to a no-value: it unwinds into the claimed executor's recover
// guard and is terminal. Generated BAML v0.223 exposes no public "not parseable yet"
// sentinel, so this normalization belongs to the BAML-linked closure — never to the
// BAML-free cadence, which knows nothing about any parser sentinel.
//
// The prefix string is BORROWED for the duration of the call; an implementation that
// retains it must copy.
type BAMLStreamParse func(ctx context.Context, prefix string) (BAMLStreamPrefixResult, error)

// BAMLStreamFinalParse runs BAML's `Parse.<Method>` — the FINAL parser, never
// `ParseStream.<Method>` — over the COMPLETE accumulated parseable text. There is no valid
// final "no value" state on a claimed stream: a nil error must come with a value, and any
// error (or a missing value) is TERMINAL for the stream.
type BAMLStreamFinalParse func(ctx context.Context, full string) (any, error)

// NativeStreamDecode decodes native canonical JSON into the STANDARD generated method's
// concrete stream/final carrier, so a chosen native value is accepted by that method's
// NewResultFunc. The standard lane supplies its own pair (the same proven
// DecodeStaticAliasStream / DecodeStaticAliasFinal cores the generated /call seam uses)
// rather than reusing the hermetic emitted spine carrier, whose Go type belongs to a
// different package.
type NativeStreamDecode func(canonicalJSON []byte) (any, error)

// NativeStaticStreamOracleInvocation is the neutral (no nanollm type, no internal/* type,
// no BAML type crosses it) but SENSITIVE request description the standard generated static
// stream seam hands the injected U1s oracle implementation.
//
// Every routing fact here MUST be TRUTHFUL: they are the parity gates that make a
// request-scoped near-miss — a client override, a fallback/round-robin/retry strategy, a
// rewrite/proxy target, a client_registry, a dynamic output schema — decline PRE-SOCKET
// instead of claiming an irreversible stream.
type NativeStaticStreamOracleInvocation struct {
	// Method is the BAML function name being served.
	Method string
	// Descriptor is the FRESH promptdescriptor.Function the generated method selected for
	// Method. Carried for the request facts only: the oracle lane's native plan is built
	// from the executor's BAKED registry descriptor, so a deployment mutation that changed
	// only BAML's plan is caught by the live plan compare rather than absorbed.
	Descriptor promptdescriptor.Function
	// Args / ArgOrder are the exact generated argument map + declared order (the BAML
	// request facts).
	Args     map[string]any
	ArgOrder []string
	// Values is the PROJECTED argument vector — the ordered, already-typed neutral value
	// tree the generated projector produced, and the ONLY host-value input native binding
	// accepts. SENSITIVE.
	Values []promptdescriptor.ArgumentValue
	// Mode is the bounded public streaming mode. Only the two REAL streaming modes reach
	// this seam; a unary call bridged through the StreamRequest builder never installs it.
	Mode NativeStreamMode

	// Provider is the resolved leaf provider; ClientOverride is the concrete selected
	// child/leaf client name, empty for a default-client request.
	Provider       string
	ClientOverride string

	// SingleLeaf / HasFallbackChain / HasRoundRobin / HasRequestRetryOverride are the
	// whole-orchestration-plan shapes the exact cohort does not prove. Any of them declines
	// at the strategy gate BEFORE the claim.
	SingleLeaf              bool
	HasFallbackChain        bool
	HasRoundRobin           bool
	HasRequestRetryOverride bool

	// HasClientRegistryOverride / HasDynamicOutputSchema are the request-scoped exact-cohort
	// declines (default client + static schema only), read where the request adapter is
	// authoritative rather than re-derived downstream.
	HasClientRegistryOverride bool
	HasDynamicOutputSchema    bool

	// WouldRewriteOrProxy reports, for the request's EFFECTIVE send target, whether the
	// effective llmhttp client would rewrite the outbound URL or route it through a proxy at
	// EXECUTION time. A true verdict — or a nil predicate, which cannot verify the target —
	// declines pre-claim.
	WouldRewriteOrProxy func(effectiveURL string) bool

	// NeedsRaw mirrors the /stream-with-raw endpoint; IncludeReasoning is the per-request
	// reasoning-channel opt-in.
	NeedsRaw         bool
	IncludeReasoning bool

	// BuildBAMLStreamRequest builds BAML's `StreamRequest.<Method>` plan for THIS selected
	// child WITHOUT sending. It is the live plan-compare oracle the standard worker restores
	// (the BAML-free native-only lane has no such closure). It opens NO socket; a nil
	// closure, a build error, a panic, or any byte mismatch declines PRE-CLAIM.
	BuildBAMLStreamRequest func(ctx context.Context) (*llmhttp.Request, error)

	// BAMLStreamParse is the per-prefix BAML oracle and BAMLFinalParse the final one. Both
	// are BAML-ONLY: they parse, they never send. Both MUST be non-nil before the claim.
	BAMLStreamParse BAMLStreamParse
	BAMLFinalParse  BAMLStreamFinalParse

	// DecodeNativeStreamPartial / DecodeNativeStreamFinal decode native canonical JSON into
	// THIS standard method's concrete stream / final carriers. Both MUST be non-nil before
	// the claim.
	DecodeNativeStreamPartial NativeStreamDecode
	DecodeNativeStreamFinal   NativeStreamDecode

	// SendHeaders is the idempotent first-2xx liveness signal the transport fires so the
	// pool's hung detector observes liveness on a slow body; SendFirstBody is the idempotent
	// first-raw-body-byte signal. Both are best-effort.
	SendHeaders   func()
	SendFirstBody func()
}

// NativeStaticStreamOracleServeDisposition is the tri-state outcome of one U1s serve. The
// zero value is the SAFE pre-socket decline (zero sockets, zero public events).
type NativeStaticStreamOracleServeDisposition uint8

const (
	// NativeStaticStreamOracleDeclined certifies NO provider socket and NO emitted event.
	// It is the ONLY fallback-legal outcome: the already-running BAML orchestrator serves
	// the same child once.
	NativeStaticStreamOracleDeclined NativeStaticStreamOracleServeDisposition = iota
	// NativeStaticStreamOracleSucceeded means every public event was already delivered
	// through the emit sink and the final is resolved. Terminal — never a resend.
	NativeStaticStreamOracleSucceeded
	// NativeStaticStreamOracleFailed is terminal: a socket and public events MAY already
	// exist, so it can NEVER be reclassified as a decline.
	NativeStaticStreamOracleFailed
)

// NativeStaticStreamOracleServeResult is the neutral tri-state result the standard composite
// returns to the generated stream seam. Final/Raw/Reasoning are SENSITIVE;
// Stage/Reason/WinnerEngine are bounded, secret-free tokens.
type NativeStaticStreamOracleServeResult struct {
	Disposition NativeStaticStreamOracleServeDisposition

	// Succeeded-only: the ALREADY-ORACLED typed final (this method's concrete final
	// carrier, decoded by DecodeNativeStreamFinal or produced by BAMLFinalParse), the
	// accumulated /stream-with-raw channels, and the bounded winner-engine token.
	Final        any
	Raw          string
	Reasoning    string
	WinnerEngine string

	// Declined (typed pre-socket decline) or Failed-after-claim (typed terminal error).
	Err           error
	RawDiagnostic string
	Stage         string
	Reason        string
}

// NativeStaticStreamOracleServeFunc serves ONE admitted standard static
// `/stream{,-with-raw}` request through the U1s per-prefix + final BAML oracle, delivering
// every resolved public event through emit before it returns, or declines PRE-SOCKET so the
// orchestrator runs BAML for the same child exactly once.
//
// Installed AND enabled only in a serve deploy profile with the umbrella flag on; nil in
// every default production build and every flag-off build, where the generated seam retains
// the legacy [NativeStaticStreamServeFunc] installer.
type NativeStaticStreamOracleServeFunc func(ctx context.Context, inv NativeStaticStreamOracleInvocation, emit NativeSpineStreamEmit) NativeStaticStreamOracleServeResult
