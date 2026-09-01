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
func NewWorkerRuntime(proj projectdescriptor.Project, candidates []UnaryRegistration, exact *llmhttp.ExactExecutor) (worker.Runtime, error) {
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
	accepted := make([]UnaryRegistration, 0, len(candidates))
	for i := range candidates {
		c := candidates[i]
		if c.BuildMethod == nil {
			return nil, fmt.Errorf("nativespine: candidate %q has a nil BuildMethod (callback-before-claim)", c.Binding.Method)
		}
		// A duplicate candidate method is a corrupt candidate list (the generator
		// must emit each method once); fail boot rather than silently dropping one.
		// Record every non-empty candidate name BEFORE classification, so a duplicate
		// is a hard failure regardless of whether either copy is accepted or is an
		// out-of-cohort miss — a cohort-miss `continue` must never let a duplicate
		// slip past this check.
		if c.Binding.Method != "" {
			if seen[c.Binding.Method] {
				return nil, fmt.Errorf("nativespine: duplicate candidate method %q", c.Binding.Method)
			}
			seen[c.Binding.Method] = true
		}
		_, rej := classifyBinding(proj, byName, capByName, c.Binding)
		if rej != nil {
			if rej.kind == rejectHard {
				return nil, rej.err
			}
			// Expected cohort miss: OMIT from the runtime registry. This is the single
			// widening point — a later slice that admits this shape flips it to accepted
			// with no bootstrap change.
			continue
		}
		accepted = append(accepted, c)
	}
	if len(accepted) == 0 {
		return nil, fmt.Errorf("nativespine: no candidate method is in the exact U1 population (empty accepted cohort); refusing to boot a native-only worker that would decline every request")
	}

	bindings := make([]bamlutils.NativeSpineUnaryBinding, len(accepted))
	for i := range accepted {
		bindings[i] = accepted[i].Binding
	}
	// The strict executor re-classifies exactly these accepted bindings (single
	// source of truth); they were classified accepted above, so registration
	// succeeds. This keeps the executor's registry and the runtime's method maps in
	// lockstep by construction.
	exec, err := NewUnaryExecutor(proj, bindings, exact)
	if err != nil {
		return nil, err
	}

	methods := make(map[string]bamlutils.StreamingMethod, len(accepted))
	parseMethods := make(map[string]bamlutils.ParseMethod, len(accepted))
	for i := range accepted {
		sm, pm := accepted[i].BuildMethod(exec)
		name := accepted[i].Binding.Method
		// A builder that returns an incomplete method would boot a runtime that panics
		// on the first matching request (a nil Impl/constructor is a nil-func call). Fail
		// boot instead — callback-before-claim, in lockstep with the nil-BuildMethod
		// guard above.
		if sm.Impl == nil || sm.MakeInput == nil || sm.MakeOutput == nil {
			return nil, fmt.Errorf("nativespine: candidate %q built an incomplete StreamingMethod (nil Impl/MakeInput/MakeOutput)", name)
		}
		if pm.Impl == nil || pm.MakeOutput == nil {
			return nil, fmt.Errorf("nativespine: candidate %q built an incomplete ParseMethod (nil Impl/MakeOutput)", name)
		}
		methods[name] = sm
		parseMethods[name] = pm
	}
	return &workerRuntime{methods: methods, parseMethods: parseMethods}, nil
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
