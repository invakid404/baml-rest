package spine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/nativeserve/spine"
)

// jsonAliasReg is the accepted emitted candidate for the exact five-arm JSON
// alias method (the real production binding + method builder).
func jsonAliasReg() spine.UnaryRegistration {
	return spine.UnaryRegistration{
		Binding:     nativespinejsonfixture.Binding(),
		BuildMethod: nativespinejsonfixture.BuildMethod,
	}
}

// aliasType is the exact five-arm JSON recursive alias (the admitted cohort's
// return), shared by the multi-method corpus below.
const aliasType = "type JSON = int | string | bool | JSON[] | map<string, JSON>"

// runtimeWorkerNamer is the local interface the concrete workerRuntime satisfies
// so a test can read the admitted method set without exporting the concrete type.
type runtimeWorkerNamer interface {
	MethodNames() []string
}

// TestNewWorkerRuntime_AdmitsExactJSONAlias proves the happy path: a validated
// admitted project + the emitted JSON-alias candidate yield a worker.Runtime whose
// registry holds exactly the admitted method, with call + parse lookup and a
// non-panicking InitRuntime.
func TestNewWorkerRuntime_AdmitsExactJSONAlias(t *testing.T) {
	proj := jsonAliasProject(t)
	rt, err := spine.NewWorkerRuntime(proj, []spine.UnaryRegistration{jsonAliasReg()}, nil)
	if err != nil {
		t.Fatalf("NewWorkerRuntime: %v", err)
	}
	rt.InitRuntime() // must not panic on a non-empty registry

	if _, ok := rt.Method(nativespinejsonfixture.MethodName); !ok {
		t.Fatalf("admitted method %q not found via Method()", nativespinejsonfixture.MethodName)
	}
	if _, ok := rt.ParseMethod(nativespinejsonfixture.MethodName); !ok {
		t.Fatalf("admitted method %q not found via ParseMethod()", nativespinejsonfixture.MethodName)
	}
	// Default-deny: an unregistered method does not exist to the handler.
	if _, ok := rt.Method("NotAdmitted"); ok {
		t.Fatalf("unregistered method resolved via Method() — registry must be default-deny")
	}
	if _, ok := rt.ParseMethod("NotAdmitted"); ok {
		t.Fatalf("unregistered method resolved via ParseMethod() — registry must be default-deny")
	}
	// MakeAdapter returns a pure adapter (no CFFI) carrying the request context.
	ad := rt.MakeAdapter(context.Background())
	if ad == nil {
		t.Fatalf("MakeAdapter returned nil")
	}

	namer, ok := rt.(runtimeWorkerNamer)
	if !ok {
		t.Fatalf("runtime does not expose MethodNames()")
	}
	got := namer.MethodNames()
	if len(got) != 1 || got[0] != nativespinejsonfixture.MethodName {
		t.Fatalf("MethodNames() = %v, want exactly [%q]", got, nativespinejsonfixture.MethodName)
	}
}

// TestNewWorkerRuntime_OmitsCohortMiss proves the deletion-frontier behaviour: a
// candidate whose project method is well-formed but OUTSIDE the exact cohort (a
// plain string return) is OMITTED from the runtime registry, while the admitted
// candidate is served — the boot does NOT fail on a cohort miss.
func TestNewWorkerRuntime_OmitsCohortMiss(t *testing.T) {
	proj := projectFromCorpus(t, corpus(aliasType, strings.Join([]string{
		`function StaticRecursiveAliasJSON(topic: string) -> JSON { client C prompt #"{{ topic }}"# }`,
		`function PlainString(topic: string) -> string { client C prompt #"{{ topic }}"# }`,
	}, "\n")))

	cohortMiss := spine.UnaryRegistration{
		// A renamed binding whose BuildMethod is never invoked (the method is omitted).
		Binding:     renameBinding(nativespinejsonfixture.Binding(), "PlainString"),
		BuildMethod: nativespinejsonfixture.BuildMethod,
	}
	admitted := spine.UnaryRegistration{
		Binding:     renameBinding(nativespinejsonfixture.Binding(), "StaticRecursiveAliasJSON"),
		BuildMethod: nativespinejsonfixture.BuildMethod,
	}

	rt, err := spine.NewWorkerRuntime(proj, []spine.UnaryRegistration{admitted, cohortMiss}, nil)
	if err != nil {
		t.Fatalf("NewWorkerRuntime: %v", err)
	}
	if _, ok := rt.Method("StaticRecursiveAliasJSON"); !ok {
		t.Fatalf("admitted method omitted")
	}
	if _, ok := rt.Method("PlainString"); ok {
		t.Fatalf("cohort-miss method PlainString was admitted — a non-JSON-alias return must be omitted")
	}
	namer := rt.(runtimeWorkerNamer)
	if got := namer.MethodNames(); len(got) != 1 || got[0] != "StaticRecursiveAliasJSON" {
		t.Fatalf("MethodNames() = %v, want exactly [StaticRecursiveAliasJSON]", got)
	}
}

// TestNewWorkerRuntime_EmptyCohortFails proves a native-only worker that admits
// nothing refuses to boot rather than silently declining every request.
func TestNewWorkerRuntime_EmptyCohortFails(t *testing.T) {
	proj := projectFromCorpus(t, corpus("", `function F(topic: string) -> string { client C prompt #"{{ topic }}"# }`))
	cand := spine.UnaryRegistration{
		Binding:     renameBinding(nativespinejsonfixture.Binding(), "F"),
		BuildMethod: nativespinejsonfixture.BuildMethod,
	}
	_, err := spine.NewWorkerRuntime(proj, []spine.UnaryRegistration{cand}, nil)
	if err == nil {
		t.Fatalf("NewWorkerRuntime admitted an all-cohort-miss candidate set, want empty-cohort failure")
	}
	if !strings.Contains(err.Error(), "empty accepted cohort") {
		t.Fatalf("error = %v, want an empty-cohort failure", err)
	}
}

// TestNewWorkerRuntime_DuplicateCandidateFails proves a corrupt candidate list (the
// same method twice) fails boot rather than silently dropping one.
func TestNewWorkerRuntime_DuplicateCandidateFails(t *testing.T) {
	proj := jsonAliasProject(t)
	_, err := spine.NewWorkerRuntime(proj, []spine.UnaryRegistration{jsonAliasReg(), jsonAliasReg()}, nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate candidate") {
		t.Fatalf("error = %v, want a duplicate-candidate failure", err)
	}
}

// TestNewWorkerRuntime_DuplicateCohortMissFailsBoot proves two candidates for the
// SAME out-of-cohort method are a hard duplicate failure even though each on its own
// is a cohort miss — a cohort-miss `continue` must never let a corrupt duplicate
// list slip past while another candidate is accepted and the worker boots.
func TestNewWorkerRuntime_DuplicateCohortMissFailsBoot(t *testing.T) {
	proj := projectFromCorpus(t, corpus(aliasType, strings.Join([]string{
		`function StaticRecursiveAliasJSON(topic: string) -> JSON { client C prompt #"{{ topic }}"# }`,
		`function PlainString(topic: string) -> string { client C prompt #"{{ topic }}"# }`,
	}, "\n")))
	admitted := spine.UnaryRegistration{
		Binding:     renameBinding(nativespinejsonfixture.Binding(), "StaticRecursiveAliasJSON"),
		BuildMethod: nativespinejsonfixture.BuildMethod,
	}
	miss := spine.UnaryRegistration{
		Binding:     renameBinding(nativespinejsonfixture.Binding(), "PlainString"),
		BuildMethod: nativespinejsonfixture.BuildMethod,
	}
	_, err := spine.NewWorkerRuntime(proj, []spine.UnaryRegistration{admitted, miss, miss}, nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate candidate") {
		t.Fatalf("error = %v, want a duplicate-candidate hard failure for a repeated cohort-miss method", err)
	}
}

// TestNewWorkerRuntime_HardFailsOnCorruptClientReference proves a candidate whose
// project method references a client ABSENT from the project client graph is
// structural corruption (Project.Validate does not catch it), so it must fail boot
// HARD — never be downgraded to a cohort miss and omitted. Proven both for a lone
// corrupt candidate AND mixed with a valid one.
func TestNewWorkerRuntime_HardFailsOnCorruptClientReference(t *testing.T) {
	const ghost = "GhostClientNotInGraph"

	t.Run("lone_corrupt_candidate", func(t *testing.T) {
		proj := mutatedJSONProject(t, func(p *projectdescriptor.Project) {
			for i := range p.Methods {
				p.Methods[i].Client = ghost
			}
		})
		_, err := spine.NewWorkerRuntime(proj, []spine.UnaryRegistration{jsonAliasReg()}, nil)
		if err == nil || !strings.Contains(err.Error(), "not present in the project client graph") {
			t.Fatalf("error = %v, want a hard client-reference-corruption failure", err)
		}
		if strings.Contains(err.Error(), "empty accepted cohort") {
			t.Fatalf("corrupt candidate was downgraded to a cohort miss (empty cohort): %v", err)
		}
	})

	t.Run("mixed_valid_plus_corrupt", func(t *testing.T) {
		proj := mutatedJSONProject(t, func(p *projectdescriptor.Project) {
			corrupt := p.Methods[0]
			corrupt.Name = "CorruptClientRef"
			corrupt.Client = ghost // absent from the client graph
			corrupt.Return.Method = "CorruptClientRef"
			p.Methods = append(p.Methods, corrupt)
			cap := p.Capabilities[0]
			cap.Method = "CorruptClientRef"
			p.Capabilities = append(p.Capabilities, cap)
		})
		valid := jsonAliasReg() // StaticRecursiveAliasJSON — would be accepted
		corrupt := spine.UnaryRegistration{
			Binding:     renameBinding(nativespinejsonfixture.Binding(), "CorruptClientRef"),
			BuildMethod: nativespinejsonfixture.BuildMethod,
		}
		_, err := spine.NewWorkerRuntime(proj, []spine.UnaryRegistration{valid, corrupt}, nil)
		if err == nil {
			t.Fatalf("NewWorkerRuntime BOOTED with a corrupt candidate present; a corrupt candidate must fail boot, not be omitted")
		}
		if !strings.Contains(err.Error(), "not present in the project client graph") {
			t.Fatalf("error = %v, want the client-reference-corruption hard failure", err)
		}
	})
}

// TestNewWorkerRuntime_HardFailsOnDanglingReturnReference proves a candidate whose
// return descriptor references a type absent from its own bundle is structural
// corruption that schema.FromStaticDescriptor (via Bundle.Validate) rejects and
// proj.Validate does NOT — so it must fail boot HARD, never be downgraded to a cohort
// miss and omitted. Proven mixed with a valid candidate: the worker must not boot
// serving only the valid method while silently dropping the corrupt one.
func TestNewWorkerRuntime_HardFailsOnDanglingReturnReference(t *testing.T) {
	proj := mutatedJSONProject(t, func(p *projectdescriptor.Project) {
		corrupt := p.Methods[0]
		corrupt.Name = "DanglingReturnRef"
		// Keep the envelope (return method/version) consistent so the failure is the
		// return-type lowering, not the envelope guard. Drop the recursive-alias
		// definitions the return still references, leaving a dangling cross-reference.
		corrupt.Return.Method = "DanglingReturnRef"
		corrupt.Return.StructuralRecursiveAliases = nil
		p.Methods = append(p.Methods, corrupt)
		cap := p.Capabilities[0]
		cap.Method = "DanglingReturnRef"
		p.Capabilities = append(p.Capabilities, cap)
	})
	valid := jsonAliasReg() // StaticRecursiveAliasJSON — would be accepted
	corrupt := spine.UnaryRegistration{
		Binding:     renameBinding(nativespinejsonfixture.Binding(), "DanglingReturnRef"),
		BuildMethod: nativespinejsonfixture.BuildMethod,
	}
	_, err := spine.NewWorkerRuntime(proj, []spine.UnaryRegistration{valid, corrupt}, nil)
	if err == nil {
		t.Fatalf("NewWorkerRuntime BOOTED with a dangling-return-reference candidate; structural return corruption must fail boot, not be omitted")
	}
	if !strings.Contains(err.Error(), "lower return bundle") {
		t.Fatalf("error = %v, want the return-lowering hard failure", err)
	}
	if strings.Contains(err.Error(), "empty accepted cohort") {
		t.Fatalf("corrupt candidate was downgraded to a cohort miss (empty cohort): %v", err)
	}
}

// TestNewWorkerRuntime_RoundRobinStrategyClientIsOmitted proves a WELL-FORMED
// round-robin strategy method — its client is a strategy WRAPPER present in Clients
// (as BuildClientGraph always emits) AND registered in Strategies — is a population
// decline: omitted at boot (NOT a hard failure, and NOT because a shared-state advancer
// is attached). Mixed with a valid candidate, the worker boots serving only the valid
// direct-client method.
func TestNewWorkerRuntime_RoundRobinStrategyClientIsOmitted(t *testing.T) {
	proj := mutatedJSONProject(t, func(p *projectdescriptor.Project) {
		leafClient := p.Methods[0].Client
		// The strategy WRAPPER's own Clients entry (BuildClientGraph inserts every
		// client — strategy wrappers included — into Clients), so the method's client
		// RESOLVES and it is an out-of-cohort population miss, not corruption.
		wrapper := p.Clients[0]
		wrapper.Config.Name = "RRStrategy"
		p.Clients = append(p.Clients, wrapper)
		p.Strategies = append(p.Strategies, projectdescriptor.Strategy{
			Name:     "RRStrategy",
			Kind:     projectdescriptor.StrategyRoundRobin,
			Children: []string{leafClient},
		})
		rr := p.Methods[0]
		rr.Name = "RoundRobinMethod"
		rr.Client = "RRStrategy"
		rr.Return.Method = "RoundRobinMethod"
		p.Methods = append(p.Methods, rr)
		cap := p.Capabilities[0]
		cap.Method = "RoundRobinMethod"
		p.Capabilities = append(p.Capabilities, cap)
	})
	valid := jsonAliasReg()
	rr := spine.UnaryRegistration{
		Binding:     renameBinding(nativespinejsonfixture.Binding(), "RoundRobinMethod"),
		BuildMethod: nativespinejsonfixture.BuildMethod,
	}
	rt, err := spine.NewWorkerRuntime(proj, []spine.UnaryRegistration{valid, rr}, nil)
	if err != nil {
		t.Fatalf("a WELL-FORMED round-robin strategy method (wrapper present in Clients) must be OMITTED (population miss), not fail boot or empty the cohort: %v", err)
	}
	namer, ok := rt.(runtimeWorkerNamer)
	if !ok {
		t.Fatalf("runtime %T does not expose MethodNames", rt)
	}
	names := namer.MethodNames()
	if len(names) != 1 {
		t.Fatalf("admitted methods = %v, want exactly the valid direct-client method (round-robin strategy method omitted)", names)
	}
	for _, n := range names {
		if n == "RoundRobinMethod" {
			t.Fatalf("round-robin strategy method was admitted; it must be omitted as a population miss")
		}
	}
}

// TestNewWorkerRuntime_HardFailsOnStrategyWithoutClientWrapper proves the CORRUPT
// counterpart: a method whose client names a Strategies entry that has NO matching
// Client wrapper in Clients — a shape BuildClientGraph never emits (it inserts every
// client, strategy wrappers included, into Clients) — must fail boot HARD, never be
// omitted as a population miss. Proven mixed with a valid candidate.
func TestNewWorkerRuntime_HardFailsOnStrategyWithoutClientWrapper(t *testing.T) {
	proj := mutatedJSONProject(t, func(p *projectdescriptor.Project) {
		leafClient := p.Methods[0].Client
		// A Strategies entry with NO corresponding Clients wrapper — structural
		// corruption, not a legitimate strategy.
		p.Strategies = append(p.Strategies, projectdescriptor.Strategy{
			Name:     "GhostStrategyNoWrapper",
			Kind:     projectdescriptor.StrategyRoundRobin,
			Children: []string{leafClient},
		})
		corrupt := p.Methods[0]
		corrupt.Name = "CorruptStrategyRef"
		corrupt.Client = "GhostStrategyNoWrapper" // in Strategies, absent from Clients
		corrupt.Return.Method = "CorruptStrategyRef"
		p.Methods = append(p.Methods, corrupt)
		cap := p.Capabilities[0]
		cap.Method = "CorruptStrategyRef"
		p.Capabilities = append(p.Capabilities, cap)
	})
	valid := jsonAliasReg()
	corrupt := spine.UnaryRegistration{
		Binding:     renameBinding(nativespinejsonfixture.Binding(), "CorruptStrategyRef"),
		BuildMethod: nativespinejsonfixture.BuildMethod,
	}
	_, err := spine.NewWorkerRuntime(proj, []spine.UnaryRegistration{valid, corrupt}, nil)
	if err == nil {
		t.Fatalf("NewWorkerRuntime BOOTED with a strategy-without-wrapper candidate; a Strategies entry whose Client is absent from the graph is corruption and must fail boot, not be omitted")
	}
	if !strings.Contains(err.Error(), "not present in the project client graph") {
		t.Fatalf("error = %v, want the client-reference-corruption hard failure", err)
	}
	if strings.Contains(err.Error(), "empty accepted cohort") {
		t.Fatalf("corrupt candidate was downgraded to a cohort miss (empty cohort): %v", err)
	}
}

// TestNewWorkerRuntime_IncompleteBuiltMethodFailsBoot proves a builder that returns an
// incomplete StreamingMethod/ParseMethod (a nil required callback) fails boot rather
// than publishing a method map that panics on the first matching request.
func TestNewWorkerRuntime_IncompleteBuiltMethodFailsBoot(t *testing.T) {
	proj := jsonAliasProject(t)
	reg := jsonAliasReg()
	reg.BuildMethod = func(bamlutils.NativeSpineUnaryExecutor) (bamlutils.StreamingMethod, bamlutils.ParseMethod) {
		// A builder that forgets the required callbacks: a runtime that published this
		// would panic on the first request instead of failing boot.
		return bamlutils.StreamingMethod{}, bamlutils.ParseMethod{}
	}
	_, err := spine.NewWorkerRuntime(proj, []spine.UnaryRegistration{reg}, nil)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("error = %v, want an incomplete-built-method boot failure", err)
	}
}

// TestNewWorkerRuntime_HardFailures proves genuine corruption fails boot (never a
// silent omission): a candidate naming a method the project did not admit, a nil
// BuildMethod, and a nil projector callback.
func TestNewWorkerRuntime_HardFailures(t *testing.T) {
	proj := jsonAliasProject(t)

	t.Run("unknown_method", func(t *testing.T) {
		cand := spine.UnaryRegistration{
			Binding:     renameBinding(nativespinejsonfixture.Binding(), "NopeNotInProject"),
			BuildMethod: nativespinejsonfixture.BuildMethod,
		}
		_, err := spine.NewWorkerRuntime(proj, []spine.UnaryRegistration{cand}, nil)
		if err == nil || !strings.Contains(err.Error(), "did not admit") {
			t.Fatalf("error = %v, want a name-mismatch hard failure", err)
		}
	})

	t.Run("nil_build_method", func(t *testing.T) {
		cand := spine.UnaryRegistration{Binding: nativespinejsonfixture.Binding(), BuildMethod: nil}
		_, err := spine.NewWorkerRuntime(proj, []spine.UnaryRegistration{cand}, nil)
		if err == nil || !strings.Contains(err.Error(), "nil BuildMethod") {
			t.Fatalf("error = %v, want a nil-BuildMethod hard failure", err)
		}
	})

	t.Run("nil_projector", func(t *testing.T) {
		b := nativespinejsonfixture.Binding()
		b.ProjectInput = nil
		cand := spine.UnaryRegistration{Binding: b, BuildMethod: nativespinejsonfixture.BuildMethod}
		_, err := spine.NewWorkerRuntime(proj, []spine.UnaryRegistration{cand}, nil)
		if err == nil || !strings.Contains(err.Error(), "ProjectInput is nil") {
			t.Fatalf("error = %v, want a nil-projector hard failure", err)
		}
	})
}

// compile-time assertion the emitted (Binding, BuildMethod) pair is exactly the
// UnaryRegistration field shape (catches emitted-signature drift at build time).
var _ = spine.UnaryRegistration{
	Binding:     nativespinejsonfixture.Binding(),
	BuildMethod: nativespinejsonfixture.BuildMethod,
}
