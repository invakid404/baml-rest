package spine

import (
	"context"
	"fmt"
	"sort"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/worker"
)

// UnaryRegistration is ONE emitted per-method candidate the production
// native-only worker offers to the population classifier: the neutral
// projector/decoder binding plus the emitted BuildMethod closure that turns a
// neutral executor into the (StreamingMethod, ParseMethod) pair the worker
// handler dispatches through.
//
// It is deliberately a CANDIDATE, not an enrollment: a build generator emits one
// per codegen-admitted method, and membership in the served runtime is decided
// downstream by NewWorkerRuntime's single U1 classifier — not by whoever built
// the candidate list. The type carries ONLY the native BuildMethod + Binding;
// there is no fallback slot, so a BAML executor or oracle can never be injected
// through it (§4, "no fallback slot").
type UnaryRegistration struct {
	// Binding is the emitted neutral per-method registration (method name +
	// reflection-free projector + strict decoder), the same value the strict
	// executor consumes.
	Binding bamlutils.NativeSpineUnaryBinding

	// BuildMethod is the emitted method builder with the codegen BuildMethod
	// signature: it turns the shared neutral executor into the worker-dispatchable
	// StreamingMethod (admits unary call only) and ParseMethod (socket-free) for
	// this candidate's method.
	BuildMethod func(exec bamlutils.NativeSpineUnaryExecutor) (bamlutils.StreamingMethod, bamlutils.ParseMethod)
}

// StreamRegistration is ONE emitted per-method STREAM candidate (M3e-A) the production
// native-only worker offers to the population classifier: the neutral STREAM binding
// (which EMBEDS the unary projector/final decoder in Binding.Unary and adds the partial
// decoder) plus the emitted BuildMethod closure.
//
// It carries no second UnaryRegistration on purpose. Binding.Unary IS the authoritative
// unary projection for this method: Go closures have no meaningful equality operation,
// so a duplicated unary binding would create a "these must match" invariant that nothing
// could validate. The emitter makes StreamBinding() return Binding() as its Unary field,
// and the native-only candidate list consumes that one field.
//
// Like [UnaryRegistration] it is a CANDIDATE, not an enrollment: membership in the
// served runtime is decided downstream by the single classifier. There is no fallback
// slot, so a BAML executor or oracle can never be injected through it.
type StreamRegistration struct {
	// Binding is the emitted neutral per-method STREAM registration.
	Binding bamlutils.NativeSpineStreamBinding

	// BuildMethod is the emitted method builder with the codegen BuildMethod signature.
	// Its parameter type is the UNARY executor (the frozen contract the emitted module
	// names); the emitted stream arms type-assert it up to the stream executor, which is
	// what NewWorkerRuntime supplies.
	BuildMethod func(exec bamlutils.NativeSpineUnaryExecutor) (bamlutils.StreamingMethod, bamlutils.ParseMethod)
}

// workerRuntime is the immutable admitted registry that satisfies worker.Runtime
// (§4, "immutable admitted registry"). It is DEFAULT-DENY: only methods present
// in the accepted maps exist to worker.Handler; every other method/mode is
// rejected before an executor can claim or open a socket. It is built once by
// NewWorkerRuntime and never mutated afterwards — there is no env-based method
// enrollment and no per-request traffic split.
type workerRuntime struct {
	methods      map[string]bamlutils.StreamingMethod
	parseMethods map[string]bamlutils.ParseMethod
}

// compile-time assertion the runtime satisfies the neutral worker contract.
var _ worker.Runtime = (*workerRuntime)(nil)

// NewWorkerRuntime builds the immutable admitted native runtime for the exact U1
// population from a VALIDATED project plus the emitted per-method candidates. It
// is the packaged join the deletion-substrate design centers on:
//
//	project descriptor -> emitted per-method candidates -> spine-owned population
//	classifier -> immutable admitted registry -> worker.Runtime
//
// It:
//
//   - validates the whole project (version fences + capability manifest) via
//     proj.Validate;
//   - classifies EVERY candidate against the single U1 population predicate
//     (classifyBinding, the same checks the strict executor uses), FAILING BOOT on
//     any hard corruption (invalid descriptor/envelope, missing/blocked capability,
//     nil callback, name mismatch) and OMITTING expected cohort misses
//     (non-required-scalar input, unsupported client/strategy/options, a Return the
//     totality predicate declines);
//   - rejects a duplicate candidate method (a corrupt candidate list) as a hard
//     failure;
//   - fails boot on an EMPTY accepted cohort — a native-only worker that admits
//     nothing has no reason to serve and would silently decline every request;
//   - constructs ONE strict UnaryExecutor over exactly the accepted bindings, then
//     invokes ONLY the accepted candidates' BuildMethod against it to produce the
//     immutable method/parse maps.
//
// A nil exact executor uses the hardened default. Later slices grow the cohort by
// widening classifyBinding / the executor WITHOUT touching NewWorkerRuntime's
// shape, the bootstrap, the pool, the plugin ABI, or the build flag.
func NewWorkerRuntime(proj projectdescriptor.Project, candidates []StreamRegistration, exact *llmhttp.ExactExecutor) (worker.Runtime, error) {
	// Transient records, as in NewStreamExecutor: they never outlive this constructor, so
	// they may point at the caller's slice. The single detachment boundary is
	// copyStreamBinding in classifyRegistration — see the note there.
	normalized := make([]candidateRegistration, len(candidates))
	for i := range candidates {
		normalized[i] = candidateRegistration{
			binding: candidates[i].Binding.Unary,
			stream:  &candidates[i].Binding,
			build:   candidates[i].BuildMethod,
		}
	}
	accepted, err := classifyCandidates(proj, normalized, true)
	if err != nil {
		return nil, err
	}
	if len(accepted) == 0 {
		return nil, fmt.Errorf("nativespine: no candidate method is in the exact U1 stream population (empty accepted cohort); refusing to boot a native-only worker that would decline every request")
	}

	// The strict STREAM executor re-classifies exactly these accepted candidates (single
	// source of truth); they were classified accepted above, so registration succeeds.
	// This keeps the executor's registry and the runtime's method maps in lockstep by
	// construction. It is a NativeSpineStreamExecutor, so the emitted stream arms'
	// type assertion succeeds and every admitted method serves /call, /stream,
	// /stream-with-raw, and both parse routes.
	exec, err := newStreamExecutorFrom(proj, accepted, exact)
	if err != nil {
		return nil, err
	}

	methods := make(map[string]bamlutils.StreamingMethod, len(accepted))
	parseMethods := make(map[string]bamlutils.ParseMethod, len(accepted))
	for i := range accepted {
		sm, pm := accepted[i].build(exec)
		name := accepted[i].binding.Method
		// A builder that returns an incomplete method would boot a runtime that panics
		// on the first matching request (a nil Impl/constructor is a nil-func call). Fail
		// boot instead — callback-before-claim, in lockstep with the nil-BuildMethod
		// guard above. M3e-A additionally requires the FULL stream surface:
		// MakeStreamOutput (the pointer carrier constructor) and ParseMethod.StreamImpl
		// (the socket-free stream parse). A native-only worker that booted without them
		// would accept a /stream request and then fail at dispatch.
		if sm.Impl == nil || sm.MakeInput == nil || sm.MakeOutput == nil || sm.MakeStreamOutput == nil {
			return nil, fmt.Errorf("nativespine: candidate %q built an incomplete StreamingMethod (nil Impl/MakeInput/MakeOutput/MakeStreamOutput)", name)
		}
		if pm.Impl == nil || pm.MakeOutput == nil || pm.StreamImpl == nil {
			return nil, fmt.Errorf("nativespine: candidate %q built an incomplete ParseMethod (nil Impl/MakeOutput/StreamImpl)", name)
		}
		methods[name] = sm
		parseMethods[name] = pm
	}
	return &workerRuntime{methods: methods, parseMethods: parseMethods}, nil
}

// classifyCandidates validates the whole project and classifies EVERY normalized
// candidate against the single U1 population predicate (classifyRegistration — the same
// checks the strict executors use), returning the accepted subset in input order. It is
// the ONE shared selection helper NewWorkerRuntime (native-only, stream surface) and
// NewPopulationExecutor (the ExecBridge-U1c standard composite, unary surface) both
// call, so there is exactly one deletion frontier. It FAILS HARD on an invalid project,
// a nil BuildMethod, a duplicate candidate method (a corrupt candidate list), or any
// classifyRegistration rejectHard (invalid descriptor/envelope, missing/blocked
// capability, nil callback, name mismatch, a stream-stamped method the totality
// predicate declines), and OMITS ordinary cohort misses. It does NOT decide emptiness —
// the caller does: native-only refuses an empty accepted set, the standard composite
// allows it (all requests fall back to BAML).
func classifyCandidates(proj projectdescriptor.Project, candidates []candidateRegistration, requireStream bool) ([]candidateRegistration, error) {
	if err := proj.Validate(); err != nil {
		return nil, fmt.Errorf("nativespine: invalid project descriptor: %w", err)
	}
	byName := make(map[string]projectdescriptor.Method, len(proj.Methods))
	for _, m := range proj.Methods {
		byName[m.Name] = m
	}
	capByName := make(map[string]projectdescriptor.MethodCapability, len(proj.Capabilities))
	for _, c := range proj.Capabilities {
		capByName[c.Method] = c
	}

	seen := make(map[string]bool, len(candidates))
	accepted := make([]candidateRegistration, 0, len(candidates))
	for i := range candidates {
		c := candidates[i]
		if c.build == nil {
			return nil, fmt.Errorf("nativespine: candidate %q has a nil BuildMethod (callback-before-claim)", c.binding.Method)
		}
		// A duplicate candidate method is a corrupt candidate list (the generator must
		// emit each method once); fail rather than silently dropping one. Recorded BEFORE
		// classification, so a duplicate is a hard failure regardless of whether either
		// copy is accepted or is an out-of-cohort miss.
		if c.binding.Method != "" {
			if seen[c.binding.Method] {
				return nil, fmt.Errorf("nativespine: duplicate candidate method %q", c.binding.Method)
			}
			seen[c.binding.Method] = true
		}
		_, rej := classifyRegistration(proj, byName, capByName, c, requireStream)
		if rej != nil {
			if rej.kind == rejectHard {
				return nil, rej.err
			}
			// Expected cohort miss: OMIT. This is the single widening point — a later
			// slice that admits this shape flips it to accepted with no bootstrap change.
			continue
		}
		accepted = append(accepted, c)
	}
	return accepted, nil
}

// NewPopulationExecutor builds a population-filtered *UnaryExecutor over exactly the
// accepted subset of candidates — the ExecBridge-U1c standard composite's construction
// path. Unlike NewUnaryExecutor (STRICT: every passed binding must be admitted) it OMITS
// ordinary cohort misses via the shared classifyCandidates, and unlike NewWorkerRuntime
// it ALLOWS AN EMPTY accepted set: a standard artifact whose project has nothing in the
// exact U1 population yields an all-decline executor (every /call falls back to BAML),
// which is legitimate — the difference between "empty is fatal" (native-only) and "empty
// is fine" (standard) is encoded in two constructors, never an environment switch. It
// still FAILS HARD on corruption. It builds only the executor over the accepted bindings
// (never the worker method maps): the standard composite drives CallWithOracle directly,
// not the worker handler.
func NewPopulationExecutor(proj projectdescriptor.Project, candidates []UnaryRegistration, exact *llmhttp.ExactExecutor) (*UnaryExecutor, error) {
	normalized := make([]candidateRegistration, len(candidates))
	for i := range candidates {
		normalized[i] = candidateRegistration{binding: candidates[i].Binding, build: candidates[i].BuildMethod}
	}
	// requireStream=false: the standard composite needs the UNARY surface only. The
	// classifier therefore accepts BOTH classes' unary projection, which is exactly what
	// preserves U1c /call behaviour after the v3 descriptor bump — accepting a
	// ClassStaticStream method here is NOT standard stream enrollment. This constructor
	// builds only the executor: it never constructs a stream BuildMethod, installs a
	// stream factory, or exposes a standard stream executor.
	accepted, err := classifyCandidates(proj, normalized, false)
	if err != nil {
		return nil, err
	}
	bindings := make([]bamlutils.NativeSpineUnaryBinding, len(accepted))
	for i := range accepted {
		bindings[i] = accepted[i].binding
	}
	// The strict executor re-classifies exactly these accepted bindings (single source of
	// truth); they were classified accepted above, so registration succeeds — including
	// the zero-binding case, which yields an all-decline executor.
	return NewUnaryExecutor(proj, bindings, exact)
}

// InitRuntime is the pure-Go validation/no-op init: it loads no shared library
// and reads no environment, and validates the registry is non-empty so a misbuilt
// runtime fails loudly at boot rather than serving an all-decline worker.
func (r *workerRuntime) InitRuntime() {
	if len(r.methods) == 0 || len(r.parseMethods) == 0 {
		panic("nativespine: workerRuntime has an empty method registry")
	}
}

// Method returns the StreamingMethod for name, preserving the (value, ok) shape
// the worker handler's "method %q not found" contract depends on. A non-admitted
// method is default-denied here, before any executor can claim or open a socket.
func (r *workerRuntime) Method(name string) (bamlutils.StreamingMethod, bool) {
	m, ok := r.methods[name]
	return m, ok
}

// ParseMethod returns the ParseMethod for name, preserving the (value, ok) shape
// the worker handler's "parse method %q not found" contract depends on.
func (r *workerRuntime) ParseMethod(name string) (bamlutils.ParseMethod, bool) {
	m, ok := r.parseMethods[name]
	return m, ok
}

// MakeAdapter returns a pure-Go adapter carrying the request context. It links no
// CFFI — that is the whole point of the native-only runtime.
func (r *workerRuntime) MakeAdapter(ctx context.Context) bamlutils.Adapter {
	return newSpineAdapter(ctx)
}

// MethodNames returns the sorted set of admitted method names. It is the
// structurally-observable deletion frontier (§4, "deletion frontier is observable
// structurally") — a bounded count/name set for tests and boot diagnostics, never
// request data.
func (r *workerRuntime) MethodNames() []string {
	out := make([]string, 0, len(r.methods))
	for name := range r.methods {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
