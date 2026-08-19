package workerboot

// De-BAML serving cutover S1 — the factory-to-handler wiring guard.
//
// Run is monolithic (it builds every collaborator and then serves), so the link between
// "a native factory Option was supplied" and "the resulting callback reached
// worker.Config" is not reachable from a unit test without refactoring the boot path. It
// is, however, exactly the kind of link that disappears silently: a cold review showed
// that deleting one factory line left every committed test green.
//
// So this guard enumerates Options' fields and requires that each native factory is both
// BUILT (invoked with the worker registry) and THREADED (the value it produced appears in
// the worker.Config literal). It is a structural check, not a behavioural one — but it is
// the check that fails when someone adds another factory Option and forgets to pass it on.
//
// TWO REVIEW FINDINGS SHAPED WHAT FOLLOWS, AND BOTH ARE CLOSED BY CONSTRUCTION.
//
//  1. DISCOVERY used to read the source: it matched fields whose declared type was written
//     inline as `func(… prometheus.Registerer) (…)`. Declare a factory through a NAMED
//     function type and the field is not a *ast.FuncType, so discovery skipped it — and a
//     factory nobody discovers is a factory nobody checks. Discovery is now REFLECTION over
//     the compiled struct, where a named function type, an alias and an inline literal are
//     all simply reflect.Func with the same signature. There is no spelling to evade.
//
//  2. ASSOCIATION used to scan forward through the file for `<local> = fn`. Drop a
//     factory's RESULT while keeping the call and the scan walked into the NEXT factory's
//     block, found ITS assignment, and reported the dropped callback as threaded.
//     Association is now bound to the option's own `if opts.<Factory> != nil { … }` block.
//
// And the classifier is FAIL-CLOSED. Every field of Options must land in a class this
// guard understands: a registry factory (must be wired), or a func explicitly acknowledged
// as something else (and that acknowledgement is itself checked), or plain data. Anything
// else — a factory-named field that is not a function, a new function field nobody
// classified — TRIPS the guard. A future factory cannot be silently ignored, whatever it
// is called and however its type is spelled: the guard fails until someone decides.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// workerbootAST parses the boot source this guard reasons about.
func workerbootAST(t *testing.T) *ast.File {
	t.Helper()
	src, err := os.ReadFile("workerboot.go")
	if err != nil {
		t.Fatalf("read workerboot.go: %v", err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "workerboot.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse workerboot.go: %v", err)
	}
	return f
}

// acknowledgedNonFactoryFuncs names the function-typed Options fields that are NOT registry
// factories, with the reason each one is exempt from the built-and-threaded rule.
//
// This map is the guard's only escape hatch, and it is deliberately small and audited: an
// entry must name a field that still exists, and the field it names must still be invoked
// in Run. An unlisted function field is not exempt — it trips the guard.
var acknowledgedNonFactoryFuncs = map[string]string{
	"NativeInit": "initializes the native runtime at startup; takes no registry and produces " +
		"no callback to thread, so Run invokes it directly rather than wiring a result",
}

// isRegistryFactory reports whether a TYPE is a native factory: a function handed the
// worker's Prometheus registry.
//
// This is asked of reflect.Type, not of source text, which is the point. A field declared
// `NativeServeFactory func(reg prometheus.Registerer) (…)`, one declared through a named
// type `type ServeFactory func(prometheus.Registerer) (…)`, and one declared through an
// alias are indistinguishable here — all three are reflect.Func with a Registerer
// parameter. The parameter's NAME is irrelevant, as is the field's.
func isRegistryFactory(t reflect.Type) bool {
	if t == nil || t.Kind() != reflect.Func {
		return false
	}
	registerer := reflect.TypeOf((*prometheus.Registerer)(nil)).Elem()
	for i := 0; i < t.NumIn(); i++ {
		if t.In(i) == registerer {
			return true
		}
	}
	return false
}

// looksLikeAFactoryName reports whether a field is NAMED like a factory. It is used only to
// catch the inverse mistake — a factory-named field whose type is not a factory at all —
// which the type rule cannot see.
func looksLikeAFactoryName(name string) bool {
	return strings.HasPrefix(name, "Native") && strings.HasSuffix(name, "Factory")
}

// factoryWiring is what the guard could establish about ONE factory field, reading only
// the `if opts.<field> != nil { … }` block that owns it.
type factoryWiring struct {
	guarded  bool   // an `if opts.<field> != nil` block exists
	built    bool   // the factory is invoked with the worker registry inside that block
	bound    string // the identifier the call's first result is bound to (usually "fn")
	assigned string // the outer local that identifier is assigned to, inside the same block
}

// optionSelector reports whether an expression is `opts.<name>`.
func optionSelector(x ast.Expr, name string) bool {
	sel, ok := x.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "opts"
}

// isNilTestOf reports whether an occurrence of an expression is nothing but the operand of
// a `… == nil` / `… != nil` test.
func isNilTestOf(parent ast.Node, expr ast.Expr) bool {
	bin, ok := parent.(*ast.BinaryExpr)
	if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
		return false
	}
	other := bin.Y
	if bin.Y == expr {
		other = bin.X
	}
	id, ok := other.(*ast.Ident)
	return ok && id.Name == "nil"
}

// reallyUsesOption reports whether the boot source actually USES `opts.<name>` — invokes
// it, or consumes its value — rather than merely testing it for presence.
//
// This is what keeps the acknowledgement exception honest, and the distinction is the whole
// point of it. An earlier version asked only whether `opts.<name>` was MENTIONED anywhere,
// and a review found the consequence: `if opts.NativeInit != nil { … }` mentions the field
// while proving nothing about whether anything ever calls it, so deleting the real
// `opts.NativeInit()` call and keeping the nil-check left the guard green with the runtime
// never initialised. A nil-check is a question about the option, not a use of it.
func reallyUsesOption(file *ast.File, name string) bool {
	found := false
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			return false
		}
		if !found {
			if expr, ok := n.(ast.Expr); ok && optionSelector(expr, name) {
				var parent ast.Node
				if len(stack) > 0 {
					parent = stack[len(stack)-1]
				}
				// Anything that is not a bare nil test consumes the option: calling it,
				// assigning it, passing it, returning it, storing it in a literal.
				if !isNilTestOf(parent, expr) {
					found = true
				}
			}
		}
		stack = append(stack, n)
		return true
	})
	return found
}

// wiringForOption inspects the one `if opts.<option> != nil { … }` block and reports what
// happens INSIDE it. Nothing outside that block can satisfy any of the fields — which is
// the whole point: association is structural, not positional.
func wiringForOption(file *ast.File, option string, registry string) factoryWiring {
	var w factoryWiring
	ast.Inspect(file, func(n ast.Node) bool {
		if w.guarded {
			return false // the block was found; do not let a later node contribute
		}
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		bin, ok := ifStmt.Cond.(*ast.BinaryExpr)
		if !ok || bin.Op != token.NEQ || !optionSelector(bin.X, option) {
			return true
		}
		if id, ok := bin.Y.(*ast.Ident); !ok || id.Name != "nil" {
			return true
		}
		w.guarded = true

		// BUILT: `<bound>, err := opts.<option>(<registry>)`, inside this block.
		ast.Inspect(ifStmt.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
				return true
			}
			call, ok := as.Rhs[0].(*ast.CallExpr)
			if !ok || !optionSelector(call.Fun, option) {
				return true
			}
			passesRegistry := false
			for _, arg := range call.Args {
				if id, ok := arg.(*ast.Ident); ok && id.Name == registry {
					passesRegistry = true
				}
			}
			if !passesRegistry {
				return true
			}
			w.built = true
			if id, ok := as.Lhs[0].(*ast.Ident); ok {
				w.bound = id.Name
			}
			return false
		})

		// THREADED (first half): `<outer local> = <bound>`, inside this same block.
		if w.bound != "" {
			ast.Inspect(ifStmt.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || as.Tok != token.ASSIGN || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
					return true
				}
				rhs, ok := as.Rhs[0].(*ast.Ident)
				if !ok || rhs.Name != w.bound {
					return true
				}
				if lhs, ok := as.Lhs[0].(*ast.Ident); ok {
					w.assigned = lhs.Name
					return false
				}
				return true
			})
		}
		return false
	})
	return w
}

// workerConfigValues returns the identifiers used as VALUES in a worker.Config composite
// literal — i.e. what actually reaches the handler.
func workerConfigValues(file *ast.File) (map[string]bool, bool) {
	out := map[string]bool{}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Config" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "worker" {
			return true
		}
		found = true
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if id, ok := kv.Value.(*ast.Ident); ok {
				out[id.Name] = true
			}
		}
		return false
	})
	return out, found
}

// factoryWiringProblems is the guard itself: it classifies every field of an options
// struct and reports everything wrong with how the boot source wires them.
//
// It is a function rather than a test body so that its BITE can drive the real thing over
// a synthetic struct and a synthetic boot source, instead of restating the rules.
//
// The classification is total and fail-closed:
//
//	registry factory      -> must be guarded, built with the registry, assigned, threaded
//	acknowledged function -> must be named by a live acknowledgement and REALLY used
//	                         (invoked or consumed — a bare nil-check is not a use)
//	other function        -> UNHANDLED: trips
//	factory-NAMED non-func -> UNHANDLED: trips
//	anything else          -> plain data, ignored
func factoryWiringProblems(opts reflect.Type, file *ast.File, acknowledged map[string]string, registry string) (problems []string, factories int) {
	cfgValues, foundCfg := workerConfigValues(file)
	if !foundCfg {
		problems = append(problems, "no worker.Config literal found; the threading half of this guard is vacuous")
	}

	// A stale acknowledgement is a reserved hole: it would exempt a future field that
	// happens to reuse the name, without anyone deciding so.
	live := map[string]bool{}
	for i := 0; i < opts.NumField(); i++ {
		live[opts.Field(i).Name] = true
	}
	var stale []string
	for name := range acknowledged {
		if !live[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		problems = append(problems, fmt.Sprintf("the acknowledgement list names %s, which is no longer a "+
			"field; a future field with that name would be exempted silently", name))
	}

	for i := 0; i < opts.NumField(); i++ {
		f := opts.Field(i)
		switch {
		case isRegistryFactory(f.Type):
			factories++
			w := wiringForOption(file, f.Name, registry)
			switch {
			case !w.guarded:
				problems = append(problems, fmt.Sprintf("%s is a registry factory with no `if opts.%s != nil` "+
					"block; nothing builds it", f.Name, f.Name))
			case !w.built:
				problems = append(problems, fmt.Sprintf("%s is a registry factory that is never invoked with "+
					"the worker registry", f.Name))
			case w.assigned == "":
				problems = append(problems, fmt.Sprintf("%s is invoked but its RESULT IS DROPPED inside its own "+
					"block — the callback would be nil in production even though the factory ran", f.Name))
			case !cfgValues[w.assigned]:
				problems = append(problems, fmt.Sprintf("%s is built and assigned to %s, but %s never reaches "+
					"the worker.Config literal; the callback would be silently absent in production",
					f.Name, w.assigned, w.assigned))
			}

		case f.Type.Kind() == reflect.Func:
			if _, ok := acknowledged[f.Name]; !ok {
				problems = append(problems, fmt.Sprintf("%s is a function-typed option that is neither a registry "+
					"factory nor an acknowledged exception; this guard does not know whether it must be wired, "+
					"so it FAILS CLOSED — classify it", f.Name))
				break
			}
			if !reallyUsesOption(file, f.Name) {
				problems = append(problems, fmt.Sprintf("%s is acknowledged as a non-factory option, but the boot "+
					"source never actually USES it — at most it tests it against nil; the exemption is covering "+
					"an option that does nothing", f.Name))
			}

		case looksLikeAFactoryName(f.Name):
			problems = append(problems, fmt.Sprintf("%s is NAMED like a native factory but its type is %s, not a "+
				"function taking the worker registry; this guard does not know what it is, so it FAILS CLOSED",
				f.Name, f.Type.Kind()))
		}
	}
	return problems, factories
}

func TestEveryNativeFactoryOptionIsBuiltAndThreaded(t *testing.T) {
	problems, factories := factoryWiringProblems(
		reflect.TypeOf(Options{}), workerbootAST(t), acknowledgedNonFactoryFuncs, "metricsReg")
	for _, p := range problems {
		t.Error(p)
	}
	// NON-VACUITY: reflection must actually be finding the factories it claims to check.
	if factories < 6 {
		t.Fatalf("only %d registry factories were discovered on Options; discovery is broken and this "+
			"guard is checking almost nothing", factories)
	}
}

// --- The bite --------------------------------------------------------------------------
//
// A synthetic options struct and a synthetic boot source, driven through the REAL
// factoryWiringProblems. Between them they carry every way this guard has failed or could
// fail permissively.

// namedFactory is the shape the previous discovery missed: a factory declared through a
// NAMED function type rather than an inline signature.
type namedFactory func(reg prometheus.Registerer) (func() error, error)

// aliasedFactory adds one more spelling: a named type over the same signature with a
// differently named parameter.
type aliasedFactory func(r prometheus.Registerer) (func() error, error)

type syntheticOptions struct {
	// Correctly wired, inline signature.
	NativeWiredFactory func(reg prometheus.Registerer) (func() error, error)
	// A NAMED function type, wired correctly: discovery must see it.
	NativeNamedWiredFactory namedFactory
	// A NAMED function type that is NOT wired: the review finding, and the mutation.
	NativeNamedUnwiredFactory namedFactory
	// Another spelling, invoked but with its RESULT DROPPED while the NEXT block assigns.
	NativeDroppedFactory aliasedFactory
	// Named like a factory, but not a function at all: unhandled, must trip.
	NativeBogusFactory int
	// A function option that nobody classified: unhandled, must trip.
	NativeUnclassifiedHook func() error
	// A function option that IS acknowledged: must not trip.
	NativeAcknowledgedInit func() error
	// Plain data: ignored.
	NativeEngineName string
}

const syntheticBootSource = `package workerboot

func Run(opts Options) {
	var wired, namedWired, dropped, threadedAfterDropped Callback

	if opts.NativeWiredFactory != nil {
		fn, err := opts.NativeWiredFactory(metricsReg)
		if err != nil || fn == nil {
			os.Exit(1)
		}
		wired = fn
	}

	if opts.NativeNamedWiredFactory != nil {
		fn, err := opts.NativeNamedWiredFactory(metricsReg)
		if err != nil || fn == nil {
			os.Exit(1)
		}
		namedWired = fn
	}

	// The named-type factory that discovery used to miss is NOT wired at all here.

	if opts.NativeDroppedFactory != nil {
		fn, err := opts.NativeDroppedFactory(metricsReg)
		if err != nil || fn == nil {
			os.Exit(1)
		}
		// Its result is dropped. The NEXT block assigns, which is what the old
		// forward scan latched onto.
	}

	if opts.NativeAcknowledgedInit != nil {
		if err := opts.NativeAcknowledgedInit(); err != nil {
			os.Exit(1)
		}
	}

	threadedAfterDropped = somethingElse

	worker.New(worker.Config{
		Wired:                wired,
		NamedWired:           namedWired,
		ThreadedAfterDropped: threadedAfterDropped,
	})
}
`

// syntheticBootSourceNilCheckOnly is syntheticBootSource with ONE edit: the acknowledged
// option's real call is deleted and its nil-check kept. That is the mutation a review found
// the guard sleeping through.
var syntheticBootSourceNilCheckOnly = strings.Replace(syntheticBootSource,
	`	if opts.NativeAcknowledgedInit != nil {
		if err := opts.NativeAcknowledgedInit(); err != nil {
			os.Exit(1)
		}
	}`,
	`	if opts.NativeAcknowledgedInit != nil {
		// The real call is gone. Only the presence test remains.
		bootedWithoutInit = true
	}`, 1)

// TestAcknowledgementRequiresARealUseNotANilCheck pins the exception path in BOTH
// directions: an acknowledged option that is genuinely invoked is accepted, and one that is
// only tested against nil is not.
//
// The two sources differ by exactly that one edit, so nothing else can explain the verdict
// flipping.
func TestAcknowledgementRequiresARealUseNotANilCheck(t *testing.T) {
	if syntheticBootSourceNilCheckOnly == syntheticBootSource {
		t.Fatal("the nil-check-only variant is identical to the original; the substitution missed, " +
			"so this test would compare a source against itself and prove nothing")
	}
	ack := map[string]string{"NativeAcknowledgedInit": "startup hook, not a registry factory"}
	const complaint = "NativeAcknowledgedInit is acknowledged"

	parse := func(src string) *ast.File {
		t.Helper()
		f, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse synthetic: %v", err)
		}
		return f
	}

	// DIRECTION 1 — the option is really called: accepted.
	called := parse(syntheticBootSource)
	if !reallyUsesOption(called, "NativeAcknowledgedInit") {
		t.Error("an acknowledged option that IS invoked was reported as unused")
	}
	problems, _ := factoryWiringProblems(reflect.TypeOf(syntheticOptions{}), called, ack, "metricsReg")
	if strings.Contains(strings.Join(problems, "\n"), complaint) {
		t.Errorf("the guard complained about an acknowledged option that is genuinely called:\n%s",
			strings.Join(problems, "\n"))
	}

	// DIRECTION 2 — the real call deleted, the nil-check kept: rejected.
	nilOnly := parse(syntheticBootSourceNilCheckOnly)
	if reallyUsesOption(nilOnly, "NativeAcknowledgedInit") {
		t.Error("a bare `if opts.… != nil` was accepted as a USE of the option; deleting the real call " +
			"would leave the guard green with the option never invoked")
	}
	nilOnlyProblems, _ := factoryWiringProblems(reflect.TypeOf(syntheticOptions{}), nilOnly, ack, "metricsReg")
	if !strings.Contains(strings.Join(nilOnlyProblems, "\n"), complaint) {
		t.Errorf("THE MUTATION: the real call was deleted and only the nil-check kept, and the guard "+
			"stayed quiet. Reported:\n%s", strings.Join(nilOnlyProblems, "\n"))
	}

	// The nil-check-only source must otherwise be unchanged, so the flip above is
	// attributable to that one edit and nothing else.
	for _, other := range []string{"NativeNamedUnwiredFactory is a registry factory with no",
		"NativeDroppedFactory is invoked but its RESULT IS DROPPED"} {
		if !strings.Contains(strings.Join(nilOnlyProblems, "\n"), other) {
			t.Errorf("the nil-check-only variant lost unrelated coverage (%s); the two sources differ "+
				"by more than the deleted call", other)
		}
	}

	// And the forms that ARE uses must all count, or the new rule would reject honest code:
	// a call, a value taken, an argument, a returned value.
	for _, src := range []string{
		"package p\nfunc f(opts Options) { opts.NativeAcknowledgedInit() }\n",
		"package p\nfunc f(opts Options) { g := opts.NativeAcknowledgedInit; _ = g }\n",
		"package p\nfunc f(opts Options) { register(opts.NativeAcknowledgedInit) }\n",
		"package p\nfunc f(opts Options) func() error { return opts.NativeAcknowledgedInit }\n",
		"package p\nfunc f(opts Options) { if opts.NativeAcknowledgedInit != nil { opts.NativeAcknowledgedInit() } }\n",
	} {
		if !reallyUsesOption(parse(src), "NativeAcknowledgedInit") {
			t.Errorf("a genuine use was rejected, which would fail honest code:\n%s", src)
		}
	}
	// …while every shape that is only a presence test must not.
	for _, src := range []string{
		"package p\nfunc f(opts Options) { if opts.NativeAcknowledgedInit != nil { x = 1 } }\n",
		"package p\nfunc f(opts Options) { if opts.NativeAcknowledgedInit == nil { x = 1 } }\n",
		"package p\nfunc f(opts Options) { if nil != opts.NativeAcknowledgedInit { x = 1 } }\n",
	} {
		if reallyUsesOption(parse(src), "NativeAcknowledgedInit") {
			t.Errorf("a bare nil test was counted as a use:\n%s", src)
		}
	}
}

func TestFactoryFieldDiscoveryIsFailClosedAndSpellingIndependent(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", syntheticBootSource, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse synthetic: %v", err)
	}
	ack := map[string]string{"NativeAcknowledgedInit": "startup hook, not a registry factory"}

	problems, factories := factoryWiringProblems(reflect.TypeOf(syntheticOptions{}), f, ack, "metricsReg")

	// DISCOVERY BY TYPE. Four fields are registry factories: an inline signature, two
	// through a named type, and one through a second named type. None of them is spelled
	// the same way, and the field NAMES are never consulted to find them.
	if factories != 4 {
		t.Errorf("discovery found %d registry factories in the synthetic struct, want 4; a factory "+
			"declared through a named function type is being skipped — the exact review finding", factories)
	}

	joined := strings.Join(problems, "\n")
	mustTrip := []struct{ substr, why string }{
		{"NativeNamedUnwiredFactory is a registry factory with no",
			"THE MUTATION: a factory declared through a named function type, present but never wired"},
		{"NativeDroppedFactory is invoked but its RESULT IS DROPPED",
			"a retained call whose result is dropped, while the next block assigns"},
		{"NativeBogusFactory is NAMED like a native factory but its type is int",
			"a factory-named field the guard cannot classify must fail closed"},
		{"NativeUnclassifiedHook is a function-typed option that is neither",
			"an unclassified function option must fail closed"},
	}
	for _, c := range mustTrip {
		if !strings.Contains(joined, c.substr) {
			t.Errorf("the guard did NOT trip on: %s\nreported instead:\n%s", c.why, joined)
		}
	}

	// And it must not fire on what is correct, or it would just be failing everything.
	for _, quiet := range []string{"NativeWiredFactory is", "NativeNamedWiredFactory is",
		"NativeAcknowledgedInit is", "NativeEngineName"} {
		if strings.Contains(joined, quiet) {
			t.Errorf("the guard fired on a correctly wired or plainly ignorable field (%s):\n%s", quiet, joined)
		}
	}

	// A stale acknowledgement must trip: it reserves an exemption for a name nobody
	// declares yet.
	stale := map[string]string{"NativeAcknowledgedInit": "ok", "NativeGoneFactory": "stale"}
	staleProblems, _ := factoryWiringProblems(reflect.TypeOf(syntheticOptions{}), f, stale, "metricsReg")
	if !strings.Contains(strings.Join(staleProblems, "\n"), "the acknowledgement list names NativeGoneFactory") {
		t.Error("a stale acknowledgement was accepted; it would exempt a future field of that name silently")
	}

	// An acknowledgement covering an option the boot source never uses must trip too.
	unusedAck := map[string]string{
		"NativeAcknowledgedInit": "ok",
		"NativeUnclassifiedHook": "claims to be fine, but Run never calls it",
	}
	unusedProblems, _ := factoryWiringProblems(reflect.TypeOf(syntheticOptions{}), f, unusedAck, "metricsReg")
	if !strings.Contains(strings.Join(unusedProblems, "\n"), "NativeUnclassifiedHook is acknowledged") {
		t.Error("an exemption covering an option the boot source never refers to was accepted")
	}

	// A different registry name means nothing is built: proof the registry argument is
	// part of "built" rather than decoration.
	wrongReg, _ := factoryWiringProblems(reflect.TypeOf(syntheticOptions{}), f, ack, "someOtherRegistry")
	if !strings.Contains(strings.Join(wrongReg, "\n"), "NativeWiredFactory is a registry factory that is never invoked") {
		t.Error("a factory invoked with a different registry was still reported as built")
	}
}

// TestFactoryWiringGuardBitesOnADroppedResult pins the structural ASSOCIATION on its own:
// the dropped-result case must not borrow the next block's assignment.
func TestFactoryWiringGuardBitesOnADroppedResult(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", syntheticBootSource, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse synthetic: %v", err)
	}

	dropped := wiringForOption(f, "NativeDroppedFactory", "metricsReg")
	if !dropped.guarded || !dropped.built {
		t.Fatalf("the dropped factory was not even recognised as built: %+v", dropped)
	}
	if dropped.assigned != "" {
		t.Errorf("a factory whose result is DROPPED was reported as assigned to %q — the guard walked "+
			"into another block", dropped.assigned)
	}

	wired := wiringForOption(f, "NativeWiredFactory", "metricsReg")
	cfgValues, found := workerConfigValues(f)
	if !found {
		t.Fatal("no worker.Config literal in the synthetic source")
	}
	if !wired.built || wired.assigned != "wired" || !cfgValues[wired.assigned] {
		t.Errorf("the correctly wired factory was not recognised: %+v", wired)
	}

	if w := wiringForOption(f, "NativeNamedUnwiredFactory", "metricsReg"); w.guarded || w.built {
		t.Errorf("a factory with no `if opts.… != nil` block was reported as built: %+v", w)
	}
}
