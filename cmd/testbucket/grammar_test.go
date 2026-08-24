package main

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

// This file closes the class of defect that produced P0-4 and P0-5: the
// coverage gate validated some of a final Unit's fields and silently trusted
// the rest, while renderBucket/goTestArgs turned every one of them into
// emitted `go test` semantics. Each round found another "the gate doesn't
// check field X".
//
// Rather than patch fields one at a time, the contract below classifies
// EVERY field of Unit, and the tests check the classification in both
// directions against the real renderer:
//
//   - every field is classified (reflection; a new field fails the build's
//     tests until someone says what it does),
//   - every field the renderer can turn into different commands is
//     rejected by the gate when malformed,
//   - every field classified as inert really is inert — mutating it leaves
//     the emitted invocations and script byte-identical.
//
// So the classification cannot rot into a comment nobody verifies, and a
// field added to Unit cannot quietly reach the renderer ungated.

// unitProbe doctors one field of a real unit taken from a real plan.
type unitProbe struct {
	name     string
	baseKind unitKind
	mutate   func(Unit) Unit
	// wantIn is a substring the gate's rejection must contain. Empty means
	// the mutation is legal and the gate must still accept the plan.
	wantIn string
}

// renderProbe answers one question about a field: does renderBucket read it?
//
// It supplies its OWN minimal bucket rather than reusing a real plan,
// because whether a change is VISIBLE depends on plan shape — demoting a
// count-shard only merges it into a shared invocation if some other unit
// happens to share the resulting group key. A purpose-built fixture makes
// the answer about the renderer instead of about the fixture.
type renderProbe struct {
	// units is the fixture; units[0] is the one that gets mutated.
	units  func() []Unit
	mutate func(Unit) Unit
}

type unitFieldRule struct {
	// changesCommands: renderBucket/goTestArgs can turn this field into
	// different emitted `go test` semantics.
	changesCommands bool
	// gateCheck names the validateUnitGrammar rule covering the field.
	gateCheck string
	// reason justifies having no gate check; required when gateCheck is "".
	reason string
	// probes assert the gate rejects malformed values.
	probes []unitProbe
	// render asserts the changesCommands classification against the real
	// renderer, in whichever direction it claims.
	render renderProbe
}

// Fixture units for the render probes. Two mergeable whole-package units and
// two shards of one package: enough for any grouping change to show.
func fixturePkg(dir, module string) Unit {
	p := livePkg(dir, module, modeWork, true)
	return Unit{
		ID: p.ImportPath, Kind: kindPackage, Seconds: 100,
		Packages: []LivePackage{p}, Module: p.Module, Mode: p.Mode, Count: 100,
	}
}

func fixtureAtom() Unit {
	a := livePkg("adapters/common", "adapters/common", modeOff, true)
	b := livePkg("adapters/common/codegen", "adapters/common", modeOff, true)
	return Unit{
		ID: moduleAtomID("adapters/common"), Kind: kindModuleAtom, Seconds: 56,
		Packages: []LivePackage{a, b}, Module: "adapters/common", Mode: modeOff, Count: 100,
	}
}

func fixtureShard(i int) Unit {
	p := livePkg("bamlutils/llmhttp", "bamlutils", modeWork, true)
	return Unit{
		ID: countShardID(p.ImportPath, i), Kind: kindCountShard, Seconds: 150,
		Packages: []LivePackage{p}, Module: p.Module, Mode: p.Mode,
		Count: 17, Shard: i, Shards: 6,
	}
}

func fixtureSlice() Unit {
	p := livePkg("internal/debaml", ".", modeWork, true)
	return Unit{
		ID: runSliceID(p.ImportPath, []string{"TestA", "TestB"}), Kind: kindRunSlice, Seconds: 200,
		Packages: []LivePackage{p}, Module: p.Module, Mode: p.Mode,
		Count: 100, Run: []string{"TestA", "TestB"},
	}
}

// twoMergeable is the fixture any GROUPING question needs: two units that
// the renderer coalesces into a single invocation, so anything that splits
// them shows up as a second command.
func twoMergeable() []Unit { return []Unit{fixturePkg("pool", "pool"), fixturePkg("worker", "worker")} }

var ghostPackage = livePkg("internal/ghost", ".", modeWork, true)

var unitFieldContract = map[string]unitFieldRule{
	"ID": {
		changesCommands: true,
		gateCheck:       "non-empty ID (the renderer keys solo invocations by it, so two unnamed units collapse into one command)",
		probes: []unitProbe{{
			name:     "an unnamed count-shard",
			baseKind: kindCountShard,
			mutate:   func(u Unit) Unit { u.ID = ""; return u },
			wantIn:   "has no ID",
		}},
		render: renderProbe{
			// Two shards of one package are solo invocations keyed by ID;
			// blanking one collapses its key toward the other's.
			units:  func() []Unit { return []Unit{fixtureShard(1), fixtureShard(2)} },
			mutate: func(u Unit) Unit { u.ID = ""; return u },
		},
	},
	"Kind": {
		changesCommands: true,
		gateCheck:       "one of the four known kinds (the renderer merges anything else into a shared whole-package invocation)",
		probes: []unitProbe{
			{
				name:     "a kind the renderer does not know",
				baseKind: kindPackage,
				mutate:   func(u Unit) Unit { u.Kind = unitKind("mystery"); return u },
				wantIn:   "unknown kind",
			},
			{
				name:     "a count-shard demoted to an ordinary package",
				baseKind: kindCountShard,
				mutate:   func(u Unit) Unit { u.Kind = kindPackage; return u },
				wantIn:   "carries count-shard coordinates",
			},
		},
		render: renderProbe{
			// Kind decides solo versus merged. Promoting one of two
			// mergeable units to a sub-package kind splits the single
			// coalesced command into two.
			units:  twoMergeable,
			mutate: func(u Unit) Unit { u.Kind = kindCountShard; u.Shard, u.Shards = 1, 2; return u },
		},
	},
	"Seconds": {
		changesCommands: false,
		gateCheck:       "finite and non-negative (it is what the balancer partitioned and what the bucket advertises)",
		probes: []unitProbe{{
			name:     "a weight that is not a number",
			baseKind: kindPackage,
			mutate:   func(u Unit) Unit { u.Seconds = math.NaN(); return u },
			wantIn:   "finite, non-negative number of seconds",
		}},
		render: renderProbe{
			units:  twoMergeable,
			mutate: func(u Unit) Unit { u.Seconds = math.NaN(); return u },
		},
	},
	"Estimate": {
		changesCommands: false,
		reason:          "reporting only: renderBucket copies it into the plan artifact and never into an invocation, and a wrong value can at worst mislabel a weight as measured",
		probes: []unitProbe{{
			name:     "flipping the estimated flag",
			baseKind: kindPackage,
			mutate:   func(u Unit) Unit { u.Estimate = !u.Estimate; return u },
		}},
		render: renderProbe{
			units:  twoMergeable,
			mutate: func(u Unit) Unit { u.Estimate = !u.Estimate; return u },
		},
	},
	"Packages": {
		changesCommands: true,
		gateCheck:       "arity per kind, and every package must be value-identical to a live, test-bearing package",
		probes: []unitProbe{{
			name:     "a package the tree does not have",
			baseKind: kindPackage,
			mutate:   func(u Unit) Unit { u.Packages = []LivePackage{ghostPackage}; return u },
			wantIn:   "not in the live package set",
		}},
		render: renderProbe{
			units:  twoMergeable,
			mutate: func(u Unit) Unit { u.Packages = []LivePackage{ghostPackage}; return u },
		},
	},
	"Module": {
		changesCommands: true,
		gateCheck:       "non-empty, and equal to every one of the unit's packages' module",
		probes: []unitProbe{{
			name:     "an atom pointed at the wrong module directory",
			baseKind: kindModuleAtom,
			mutate:   func(u Unit) Unit { u.Module = "nowhere"; return u },
			wantIn:   "runs from module",
		}},
		render: renderProbe{
			// Module is the cd target, but only for a GOWORK=off unit.
			units:  func() []Unit { return []Unit{fixtureAtom()} },
			mutate: func(u Unit) Unit { u.Module = "nowhere"; return u },
		},
	},
	"Mode": {
		changesCommands: true,
		gateCheck:       "a known resolution mode, equal to every one of the unit's packages' mode",
		probes: []unitProbe{
			{
				name:     "a workspace package run under GOWORK=off",
				baseKind: kindPackage,
				mutate:   func(u Unit) Unit { u.Mode = modeOff; return u },
				wantIn:   `resolves in "work"`,
			},
			{
				name:     "a mode the renderer does not know",
				baseKind: kindPackage,
				mutate:   func(u Unit) Unit { u.Mode = "bogus"; return u },
				wantIn:   "unknown resolution mode",
			},
		},
		render: renderProbe{
			units:  twoMergeable,
			mutate: func(u Unit) Unit { u.Mode = modeOff; return u },
		},
	},
	"Count": {
		changesCommands: true,
		gateCheck:       "-count >= 1 always, and >= the base count for every kind except count-shards (whose aggregate is checked at group level)",
		probes: []unitProbe{
			{
				name:     "a unit that runs no iterations at all",
				baseKind: kindPackage,
				mutate:   func(u Unit) Unit { u.Count = 0; return u },
				wantIn:   "executes nothing",
			},
			{
				name:     "a run-slice that weakens the flake sweep",
				baseKind: kindRunSlice,
				mutate:   func(u Unit) Unit { u.Count = 5; return u },
				wantIn:   "weakening the requested -count=100",
			},
		},
		render: renderProbe{
			units:  twoMergeable,
			mutate: func(u Unit) Unit { u.Count = 0; return u },
		},
	},
	"Run": {
		changesCommands: true,
		gateCheck:       "empty unless the unit is a run-slice; non-empty when it is; every name free of regex metacharacters",
		probes: []unitProbe{
			{
				name:     "a -run filter on an ordinary package",
				baseKind: kindPackage,
				mutate:   func(u Unit) Unit { u.Run = []string{"TestOne"}; return u },
				wantIn:   "carrying a -run filter",
			},
			{
				name:     "a regex metacharacter in a slice name",
				baseKind: kindRunSlice,
				mutate: func(u Unit) Unit {
					u.Run = append(append([]string(nil), u.Run...), "TestA|TestZ")
					return u
				},
				wantIn: "regex metacharacter",
			},
		},
		render: renderProbe{
			// The heart of P0-5: the renderer emits -run for ANY kind.
			units:  twoMergeable,
			mutate: func(u Unit) Unit { u.Run = []string{"TestOne"}; return u },
		},
	},
	"Shard": {
		changesCommands: false,
		gateCheck:       "within 1..Shards for a count-shard, zero for every other kind (the gate's group-completeness evidence is derived from it)",
		probes: []unitProbe{{
			name:     "shard coordinates on an ordinary package",
			baseKind: kindPackage,
			mutate:   func(u Unit) Unit { u.Shard = 3; return u },
			wantIn:   "carries count-shard coordinates",
		}},
		render: renderProbe{
			units:  twoMergeable,
			mutate: func(u Unit) Unit { u.Shard = 3; return u },
		},
	},
	"Shards": {
		changesCommands: false,
		gateCheck:       "as Shard",
		probes: []unitProbe{{
			name:     "a group width on an ordinary package",
			baseKind: kindPackage,
			mutate:   func(u Unit) Unit { u.Shards = 4; return u },
			wantIn:   "carries count-shard coordinates",
		}},
		render: renderProbe{
			units:  twoMergeable,
			mutate: func(u Unit) Unit { u.Shards = 4; return u },
		},
	},
}

// grammarProbePlan builds a plan containing all four unit kinds, so a probe
// can doctor a REAL unit of the kind it targets rather than a hand-built one
// that may not resemble what the expander emits.
func grammarProbePlan(t *testing.T) ([]LivePackage, []Bucket, map[string][]string, planOptions) {
	t.Helper()
	st := syntheticStore()
	harpoon(st, "bamlutils/llmhttp", splitCount, 6, nil)
	harpoon(st, "internal/debaml", splitRun, 3, map[string]float64{"TestA": 200, "TestB": 120, "TestC": 60})
	live, buckets, runnables := bucketsFor(t, st, expandOptions{
		Runnables: syntheticRunnables(map[string][]string{
			repoPrefix + "internal/debaml": {"TestA", "TestB", "TestC", "ExampleD"},
		}),
	})
	if err := gate(live, buckets, runnables); err != nil {
		t.Fatalf("the probe plan is not healthy to begin with: %v", err)
	}
	seen := map[unitKind]bool{}
	for _, b := range buckets {
		for _, u := range b.Units {
			seen[u.Kind] = true
		}
	}
	for _, k := range []unitKind{kindPackage, kindModuleAtom, kindCountShard, kindRunSlice} {
		if !seen[k] {
			t.Fatalf("the probe plan has no %s unit; probes targeting it would not be proving anything", k)
		}
	}
	return live, buckets, runnables, defaultPlanOptions(live)
}

// locate finds a real unit of the given kind and returns its bucket index.
func locate(t *testing.T, buckets []Bucket, kind unitKind) (int, Unit) {
	t.Helper()
	for i, b := range buckets {
		for _, u := range b.Units {
			if u.Kind == kind {
				return i, u
			}
		}
	}
	t.Fatalf("no %s unit in the probe plan", kind)
	return -1, Unit{}
}

func TestEveryUnitFieldIsClassified(t *testing.T) {
	// A field added to Unit reaches renderBucket immediately. This makes it
	// impossible to add one without saying whether the renderer can turn it
	// into different commands, and what the gate does about it.
	typ := reflect.TypeOf(Unit{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		rule, ok := unitFieldContract[name]
		if !ok {
			t.Errorf("Unit.%s is not classified in unitFieldContract; say whether the renderer can turn it into different `go test` semantics and what validateUnitGrammar does about it", name)
			continue
		}
		if rule.gateCheck == "" && rule.reason == "" {
			t.Errorf("Unit.%s has neither a gate check nor a reason it needs none", name)
		}
		if rule.changesCommands && rule.gateCheck == "" {
			t.Errorf("Unit.%s can change the emitted commands but names no gate check", name)
		}
		if rule.gateCheck != "" {
			rejecting := 0
			for _, p := range rule.probes {
				if p.wantIn != "" {
					rejecting++
				}
			}
			if rejecting == 0 {
				t.Errorf("Unit.%s claims a gate check but has no probe asserting the gate rejects a malformed value", name)
			}
		}
		if rule.render.units == nil || rule.render.mutate == nil {
			t.Errorf("Unit.%s has no render probe; the changesCommands claim would be an unverified comment", name)
		}
		if len(rule.probes) == 0 {
			t.Errorf("Unit.%s has no probes", name)
		}
	}

	// And no stale entries, so the contract cannot describe a field that no
	// longer exists.
	for name := range unitFieldContract {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("unitFieldContract describes Unit.%s, which does not exist", name)
		}
	}
}

func TestMalformedUnitFieldsAreRejectedByTheGate(t *testing.T) {
	// The forward direction: every field the contract says is gated really
	// is, on a REAL unit of the right kind taken from a REAL plan.
	live, buckets, runnables, _ := grammarProbePlan(t)

	for _, field := range sortedKeys(unitFieldContract) {
		rule := unitFieldContract[field]
		for _, probe := range rule.probes {
			if probe.wantIn == "" {
				continue
			}
			t.Run(field+"/"+probe.name, func(t *testing.T) {
				_, base := locate(t, buckets, probe.baseKind)
				doctored := replaceUnit(buckets, base.ID, probe.mutate(base))

				var err error
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("the gate PANICKED instead of reporting a coverage error: %v", r)
						}
					}()
					err = gate(live, doctored, runnables)
				}()
				if err == nil {
					t.Fatalf("the gate PASSED a plan with a malformed Unit.%s — %s", field, rule.gateCheck)
				}
				if !strings.Contains(err.Error(), probe.wantIn) {
					t.Errorf("gate message omits %q:\n%s", probe.wantIn, err.Error())
				}
			})
		}
	}
}

func TestFieldClassificationMatchesWhatTheRendererEmits(t *testing.T) {
	// The reverse direction, and the reason this is a closed loop rather
	// than a comment: every field's claim is checked against the REAL
	// renderer on a fixture built so that any read of the field shows up.
	// A field called inert must leave the emitted commands byte-identical;
	// a field called command-changing must actually change them.
	opt := defaultPlanOptions(syntheticLive())
	render := func(units []Unit) string {
		t.Helper()
		pb := renderBucket(Bucket{Index: 0, Units: units}, opt)
		blob, err := json.Marshal(struct {
			Invocations []Invocation `json:"invocations"`
			Script      string       `json:"script"`
		}{pb.Invocations, pb.Script})
		if err != nil {
			t.Fatal(err)
		}
		return string(blob)
	}

	for _, field := range sortedKeys(unitFieldContract) {
		rule := unitFieldContract[field]
		t.Run(field, func(t *testing.T) {
			base := rule.render.units()
			before := render(base)

			doctored := append([]Unit(nil), rule.render.units()...)
			doctored[0] = rule.render.mutate(doctored[0])
			after := render(doctored)

			switch {
			case rule.changesCommands && before == after:
				t.Errorf("Unit.%s is classified as feeding the renderer, but mutating it left the emitted commands identical:\n%s\n\nEither the field is inert (reclassify it) or the fixture cannot show the difference.", field, before)
			case !rule.changesCommands && before != after:
				t.Errorf("Unit.%s is classified as inert, but mutating it changed the emitted commands — it needs a gate check that reflects what it actually does:\n--- before ---\n%s\n--- after ---\n%s", field, before, after)
			}
		})
	}
}

func TestLegalMutationsStillPassTheGate(t *testing.T) {
	// The contract's "no gate check needed" entries must not be quietly
	// wrong in the other direction either: a legal value has to keep
	// passing, or the gate would be rejecting healthy plans.
	live, buckets, runnables, _ := grammarProbePlan(t)
	for _, field := range sortedKeys(unitFieldContract) {
		for _, probe := range unitFieldContract[field].probes {
			if probe.wantIn != "" {
				continue
			}
			t.Run(field+"/"+probe.name, func(t *testing.T) {
				_, base := locate(t, buckets, probe.baseKind)
				if err := gate(live, replaceUnit(buckets, base.ID, probe.mutate(base)), runnables); err != nil {
					t.Errorf("the gate rejected a legal plan: %v", err)
				}
			})
		}
	}
}
