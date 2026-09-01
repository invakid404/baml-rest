// Package spine is the ExecBridge-U1 production codegen-spine unary executor: the
// thin ownership/adaptation lane that attaches the emitted hermetic carriers to the
// EXISTING native machinery — nativeprompt render, nativebody NormalizeStaticClient,
// nanollm Prepare, execute.RunAttempt's exact one-send, debaml.ParseStaticBundleUnaryCall,
// and the emitted bamlutils.DecodeStaticAliasFinal decoder — WITHOUT any generated
// BAML or CFFI on the emitted/runtime path.
//
// It is not a second native stack. Admission reuses nativeserve/admission's
// AdmitStaticSpineClaim (the shared static building blocks minus the BAML plan-compare
// oracle); the single send reuses execute.RunAttempt / llmhttp.ExactExecutor; the
// final parse reuses internal/debaml. The only new logic is an immutable method
// registry built from the reconstructed scalar descriptor + emitted bindings, gated
// by the ONE root-owned totality predicate debaml.SupportsNativeStaticStreamBundle,
// and the tri-state claim discipline (declined-pre-socket / succeeded /
// failed-after-claim) mapped onto the neutral bamlutils.NativeSpineUnaryResult.
//
// COHORT: exactly the proven direct five-arm `JSON` recursive alias, unary final call
// + direct parse only; inputs required string/int/float/bool scalars only. Emittable
// is not population-admitted — every other shape declines at registration or, if not
// registered, at Call with a typed pre-socket decline and zero sockets.
//
// Default-deny: this runtime is constructible + exercisable, but it changes no
// default worker selection or cohort enrollment (that is U1b).
package spine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// Bounded, secret-free stage/reason tokens for the neutral tri-state result.
const (
	stageRegistry  = "registry"
	stagePreflight = "preflight"
	stageProject   = "project_input"
	stageAdmission = "admission"
	stagePlanner   = "planner"
	stageServe     = "serve"
	stageTransport = "transport"
	stageProvider  = "provider"
	stageParse     = "parse"
	stageDecode    = "decode"

	reasonUnsupportedMethod = "method_not_registered"
	reasonContextCancelled  = "context_cancelled"
	reasonProjectInputErr   = "project_input_error"
	reasonClientRegistry    = "client_registry_present"
	reasonDynamicSchema     = "dynamic_output_schema_present"
	reasonPlannerError      = "planner_error"
	reasonPlanExpired       = "plan_expired"
	reasonPanic             = "panic"
	reasonTransportError    = "transport_error"
	reasonProviderError     = "provider_error"
	reasonInvalidBody       = "malformed_2xx_body"
	reasonNativeParse       = "native_parse_error"
	reasonParseDeclined     = "native_parse_declined"
	reasonDecodeError       = "carrier_decode_error"
	reasonUnknownOutcome    = "unknown_attempt_outcome"
)

var (
	errPlanExpired    = errors.New("nativespine: prepared plan expired before the socket")
	errMalformed2xx   = errors.New("nativespine: provider returned a 2xx body the translator could not parse")
	errParseDecline   = errors.New("nativespine: native final parser declined the response shape (no BAML fallback on this cohort)")
	errClientRegistry = errors.New("nativespine: request carries a client_registry; the exact cohort serves only the descriptor's default client")
	errDynamicSchema  = errors.New("nativespine: request carries a dynamic output schema; the exact cohort is static-only")
)

// registeredMethod is one immutable, validated registry entry: the reconstructed
// descriptor, its lowered Return Bundle (the native static SAP surface), and the
// emitted projector/decoder. All resolved at registration time.
type registeredMethod struct {
	fn      promptdescriptor.Function
	bundle  *schema.Bundle
	binding bamlutils.NativeSpineUnaryBinding
}

// Metrics is a minimal, bounded observability counter set for the executor. Every
// counter uses a bounded label (the disposition), never a content-derived value. It
// is atomic so the executor may be driven concurrently.
type Metrics struct {
	declines  atomic.Int64
	claims    atomic.Int64
	sockets   atomic.Int64
	successes atomic.Int64
	failures  atomic.Int64
}

// MetricsSnapshot is a point-in-time read of the executor's bounded counters.
type MetricsSnapshot struct {
	Declines, Claims, Sockets, Successes, Failures int64
}

// Snapshot returns the current counter values.
func (m *Metrics) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		Declines:  m.declines.Load(),
		Claims:    m.claims.Load(),
		Sockets:   m.sockets.Load(),
		Successes: m.successes.Load(),
		Failures:  m.failures.Load(),
	}
}

// UnaryExecutor is the production bamlutils.NativeSpineUnaryExecutor over the exact
// five-arm `JSON` cohort. It is immutable after construction.
type UnaryExecutor struct {
	registry map[string]*registeredMethod
	exec     *llmhttp.ExactExecutor
	metrics  *Metrics
	// admitClaim is the pre-socket admission step, defaulting to
	// admission.AdmitStaticSpineClaim. It is a field only so gated tests can inject a
	// synthetic claim to drive the post-claim fault matrix deterministically; every
	// production constructor leaves the default.
	admitClaim func(ctx context.Context, in admission.StaticInput) (*admission.StaticClaim, error)
}

// compile-time assertion the executor satisfies the neutral contract.
var _ bamlutils.NativeSpineUnaryExecutor = (*UnaryExecutor)(nil)

// NewUnaryExecutor builds an immutable executor over the VALIDATED
// projectdescriptor.Project plus the emitted per-method bindings (Codex review
// finding 2: the constructor takes the whole Project, not pre-reconstructed methods,
// so registration validates against the actual project facts). For each binding it
// finds the admitted method in the Project, reconstructs + validates the scalar
// descriptor (this package's unexported reconstructFunction — which FAILS on project
// version / templates / client retry-policy / strategy rather than stripping them),
// then REJECTS, before serving begins:
//
//   - a binding naming a method the Project did not admit;
//   - a duplicate method, a binding/descriptor name mismatch, a nil
//     ProjectInput/DecodeFinal callback;
//   - a descriptor-envelope mismatch (return method/version, or a streaming return) —
//     so registration, call, and direct parse agree in lockstep (finding 3);
//   - an input outside the required-scalar cohort;
//   - a Return that does not lower, and anything the ONE root-owned totality predicate
//     (debaml.SupportsNativeStaticStreamBundle — the exact five-arm `JSON` alias)
//     declines.
//
// A nil exec uses the hardened default exact executor. Callback-before-claim: both
// closures are resolved and validated here.
func NewUnaryExecutor(proj projectdescriptor.Project, bindings []bamlutils.NativeSpineUnaryBinding, exec *llmhttp.ExactExecutor) (*UnaryExecutor, error) {
	if exec == nil {
		exec = llmhttp.NewExactExecutor(nil)
	}
	// Validate the whole Project FIRST (Codex review finding 1): the descriptor +
	// prompt-descriptor + schema versions, the method/client/retry/strategy/template
	// invariants, AND the capability manifest (every retained method covered exactly
	// once and agreeing with its admit/decline outcome). A mismatched version or a
	// missing/duplicate/inconsistent capability record is a hard error here — it never
	// reaches registration.
	if err := proj.Validate(); err != nil {
		return nil, fmt.Errorf("nativespine: invalid project descriptor: %w", err)
	}
	e := &UnaryExecutor{
		registry:   make(map[string]*registeredMethod, len(bindings)),
		exec:       exec,
		metrics:    &Metrics{},
		admitClaim: admission.AdmitStaticSpineClaim,
	}
	byName := make(map[string]projectdescriptor.Method, len(proj.Methods))
	for _, m := range proj.Methods {
		byName[m.Name] = m
	}
	capByName := make(map[string]projectdescriptor.MethodCapability, len(proj.Capabilities))
	for _, c := range proj.Capabilities {
		capByName[c.Method] = c
	}
	for i := range bindings {
		if err := e.register(proj, byName, capByName, bindings[i]); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// Metrics returns the executor's bounded counter set.
func (e *UnaryExecutor) Metrics() *Metrics { return e.metrics }

// Methods returns the sorted set of admitted method names (registration proof).
func (e *UnaryExecutor) Methods() []string {
	out := make([]string, 0, len(e.registry))
	for name := range e.registry {
		out = append(out, name)
	}
	// Map iteration order is randomized; sort so the result is deterministic and matches
	// the "sorted set" the doc promises (CodeRabbit #6).
	sort.Strings(out)
	return out
}

// rejectionKind distinguishes the TWO reasons classifyBinding can reject a
// candidate binding, which is the split the deletion-substrate design (§3.A.2 /
// §4) turns on:
//
//   - rejectHard: the registration/descriptor is CORRUPT or INCONSISTENT — an
//     invalid project, a binding naming a method the project did not admit, a
//     missing/blocked capability record, a nil callback, or a descriptor-envelope
//     mismatch. NewUnaryExecutor and NewWorkerRuntime BOTH fail boot on it: a
//     corrupt candidate is never quietly omitted.
//   - rejectCohortMiss: the binding is well-formed but its method is simply
//     OUTSIDE the exact U1 population — a non-required-scalar input, a
//     cohort-forbidden client/strategy/options, or a Return the totality predicate
//     declines. The strict NewUnaryExecutor still rejects it (its callers pass only
//     methods they mean to serve); NewWorkerRuntime OMITS it from the runtime
//     registry so later slices grow the cohort by widening this classifier without
//     touching the bootstrap.
type rejectionKind int

const (
	rejectHard rejectionKind = iota
	rejectCohortMiss
)

// bindingRejection is a classified rejection: an error plus which of the two
// kinds it is. It implements error so the strict register path can return it
// unchanged (preserving the exact messages the executor tests pin).
type bindingRejection struct {
	kind rejectionKind
	err  error
}

func (r *bindingRejection) Error() string { return r.err.Error() }
func (r *bindingRejection) Unwrap() error { return r.err }

func hardReject(err error) *bindingRejection { return &bindingRejection{kind: rejectHard, err: err} }
func cohortMiss(err error) *bindingRejection {
	return &bindingRejection{kind: rejectCohortMiss, err: err}
}

// register is the STRICT explicit-binding registration NewUnaryExecutor uses: a
// candidate that is either hard-corrupt OR an expected cohort miss is an error,
// because a NewUnaryExecutor caller passes only the bindings it means to serve.
// NewWorkerRuntime instead consults classifyBinding directly so it can omit
// cohort misses while still failing on hard corruption (§3.A.2).
func (e *UnaryExecutor) register(proj projectdescriptor.Project, byName map[string]projectdescriptor.Method, capByName map[string]projectdescriptor.MethodCapability, b bamlutils.NativeSpineUnaryBinding) error {
	if _, dup := e.registry[b.Method]; dup {
		return fmt.Errorf("nativespine: register %q: duplicate method", b.Method)
	}
	rm, rej := classifyBinding(proj, byName, capByName, b)
	if rej != nil {
		return rej.err
	}
	e.registry[b.Method] = rm
	return nil
}

// classifyBinding is the SINGLE population owner: it validates one candidate
// binding against the whole project and returns either the resolved registry
// entry (accepted) or a classified rejection (hard corruption vs expected cohort
// miss). It is the ONE place the reconstruction, envelope, client, input, and
// totality checks live, so the executor, NewWorkerRuntime, and the build
// generator cannot invent three subtly different definitions of the deletion
// frontier. It does NOT check for duplicates — that is each caller's own concern
// (register reads its live registry; NewWorkerRuntime dedupes across candidates).
func classifyBinding(proj projectdescriptor.Project, byName map[string]projectdescriptor.Method, capByName map[string]projectdescriptor.MethodCapability, b bamlutils.NativeSpineUnaryBinding) (*registeredMethod, *bindingRejection) {
	if b.Method == "" {
		return nil, hardReject(fmt.Errorf("nativespine: register: binding has no method name"))
	}
	if b.ProjectInput == nil {
		return nil, hardReject(fmt.Errorf("nativespine: register %q: binding ProjectInput is nil (callback-before-claim)", b.Method))
	}
	if b.DecodeFinal == nil {
		return nil, hardReject(fmt.Errorf("nativespine: register %q: binding DecodeFinal is nil (callback-before-claim)", b.Method))
	}
	m, ok := byName[b.Method]
	if !ok {
		return nil, hardReject(fmt.Errorf("nativespine: register %q: binding names a method the project did not admit (name mismatch or declined method)", b.Method))
	}
	// The method's capability record must exist, be admitted, and not be blocked
	// (finding 1). proj.Validate already proved the manifest is consistent; this is the
	// explicit per-method read the cohort requires. An inconsistent record is
	// CORRUPTION (hard), not a cohort miss.
	mc, ok := capByName[b.Method]
	if !ok {
		return nil, hardReject(fmt.Errorf("nativespine: register %q: no capability record in the project manifest", b.Method))
	}
	if !mc.Admitted || mc.Blocked != "" {
		return nil, hardReject(fmt.Errorf("nativespine: register %q: capability record is not admitted (admitted=%v blocked=%q)", b.Method, mc.Admitted, mc.Blocked))
	}
	// Reconstruct + validate the project/client cohort facts (version, templates,
	// client retry-policy, strategy) — fails rather than strips (finding 2).
	// reconstructFunction distinguishes POPULATION facts (templated project,
	// retrying/strategy client — an expected cohort miss) from structural CORRUPTION
	// (a client absent from the project graph, a non-static-unary admitted method —
	// a HARD boot failure). A corrupt candidate must never be silently downgraded to
	// a miss and omitted.
	fn, rerr := reconstructFunction(proj, m)
	if rerr != nil {
		wrapped := fmt.Errorf("nativespine: register %q: %w", b.Method, rerr)
		if rerr.corrupt {
			return nil, hardReject(wrapped)
		}
		return nil, cohortMiss(wrapped)
	}
	// Descriptor envelope lockstep (finding 3): registration rejects the same envelope
	// mismatches the call-time admission does, so a method rejected by call can never be
	// accepted by direct parse (which trusts the registry). An envelope mismatch is an
	// INCONSISTENT descriptor (hard), never a benign cohort miss.
	if fn.Version != promptdescriptor.Version {
		return nil, hardReject(fmt.Errorf("nativespine: register %q: descriptor version %d, want %d", b.Method, fn.Version, promptdescriptor.Version))
	}
	if fn.Return.Version != schemadescriptor.Version {
		return nil, hardReject(fmt.Errorf("nativespine: register %q: return schema version %d, want %d", b.Method, fn.Return.Version, schemadescriptor.Version))
	}
	if fn.Return.Method != fn.Method {
		return nil, hardReject(fmt.Errorf("nativespine: register %q: return names method %q (envelope mismatch)", b.Method, fn.Return.Method))
	}
	if fn.Return.Stream {
		return nil, hardReject(fmt.Errorf("nativespine: register %q: return is the streaming variant; only unary final-call is admitted", b.Method))
	}
	// The return descriptor must LOWER before any population classification.
	// schema.FromStaticDescriptor re-validates the whole return-type graph — the
	// descriptor version, every enum string, required child payloads, and (via
	// Bundle.Validate) cross-reference self-containment: dangling references, unknown
	// enums, inline cycles, duplicate rendered names, and map-key legality. proj.Validate
	// checks NONE of these, and none are population facts — a well-formed but
	// out-of-cohort return lowers successfully and is declined below by the totality
	// predicate. A lowering failure is therefore an INCONSISTENT descriptor (structural
	// corruption), so it is a HARD boot failure, never a silent cohort-miss omission.
	bundle, err := schema.FromStaticDescriptor(fn.Return)
	if err != nil {
		return nil, hardReject(fmt.Errorf("nativespine: register %q: lower return bundle: %w", b.Method, err))
	}
	// Static-client cohort: run the SAME shared client checks Call's admission uses
	// (provider, literal model, no body-affecting option, literal base_url/api_key) so
	// registration declines exactly what Call declines — a cohort-forbidden client
	// descriptor (a request body / body option / non-openai leaf / non-literal model /
	// non-literal transport) never registers. It is a cohort miss (unsupported client),
	// not corruption.
	if err := admission.CheckStaticClientCohort(fn.Provider, fn.ClientConfig); err != nil {
		return nil, cohortMiss(fmt.Errorf("nativespine: register %q: %w", b.Method, err))
	}
	if err := requiredScalarInputs(fn); err != nil {
		return nil, cohortMiss(fmt.Errorf("nativespine: register %q: %w", b.Method, err))
	}
	// The ONE root-owned totality predicate — the exact five-arm `JSON` alias family —
	// controls registration in lockstep with call and direct parse. A Return that
	// lowers cleanly but is outside that family is a cohort miss.
	if err := debaml.SupportsNativeStaticStreamBundle(bundle); err != nil {
		return nil, cohortMiss(fmt.Errorf("nativespine: register %q: not the exact five-arm JSON alias cohort: %w", b.Method, err))
	}
	return &registeredMethod{fn: fn, bundle: bundle, binding: b}, nil
}

// requiredScalarInputs enforces the ExecBridge-U1 input cohort: every argument is a
// required (non-nullable) string/int/float/bool scalar. No nullable, list, class,
// enum, map, union, or media input.
func requiredScalarInputs(fn promptdescriptor.Function) error {
	for _, a := range fn.Args {
		if a.ValueType == nil {
			return fmt.Errorf("argument %q has no resolved value type", a.Name)
		}
		vt := *a.ValueType
		if vt.Nullable {
			return fmt.Errorf("argument %q is nullable (only required scalars are admitted)", a.Name)
		}
		switch vt.Kind {
		case promptdescriptor.ValueString, promptdescriptor.ValueInt, promptdescriptor.ValueFloat, promptdescriptor.ValueBool:
		default:
			return fmt.Errorf("argument %q uses input kind %q outside the required-scalar cohort", a.Name, vt.Kind)
		}
	}
	return nil
}
