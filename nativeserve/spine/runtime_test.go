package spine_test

import (
	"context"
	"strings"
	"testing"

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
