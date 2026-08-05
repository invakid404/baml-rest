//go:build integration

package guardledger

import (
	"fmt"
	"sort"
	"strings"
)

// Fixture layout.
//
// The stock leg needs one generated BAML function per DRIVEABLE unit, and a
// constraint whose predicate raises an evaluation error — or a @assert that
// fails — rejects the WHOLE node, so a batch containing one of those would lose
// every other result in it. Each row instance is therefore placed by its
// RECORDED stock envelope:
//
//   - envPass / envFailedCheck at @check level, and envPass at @assert level,
//     ride in their group's BATCH: one parse yields every status at once, keyed
//     by the instance label; and
//   - everything else (evaluator-error, no-checks, assertion-error) gets its own
//     ISOLATED function, so its failure cannot take another row down with it.
//
// Placement is derived from the pin, and the pin is re-verified live on every
// run, so a mis-pinned row shows up as a differential failure rather than as a
// silently absent result.

const (
	// fixtureClient is the client every fixture function names. It is never
	// called — the harness drives Parse.<Method>, not the LLM — but a BAML
	// function must declare one.
	fixtureClient = "GuardLedgerClient"

	// checkBatchPrefix and assertBatchPrefix are SEPARATE per-group batches. A
	// passing @assert rides in its own class so a mis-pinned assert can only cost
	// the assert batch, never the check batch — the two failure modes stay
	// attributable instead of one poisoning the other.
	checkBatchPrefix  = "GLB_"
	assertBatchPrefix = "GLA_"
	isoPrefix         = "GLI_"
	// dupClass carries the DUPLICATE-LABEL observation: two @check attributes
	// under one label. It is not a guard witness, it is asymmetry #2 of the
	// scope — recorded here so 7.2b decides the wire shape on evidence.
	dupClass = "GLD_dup"
	// fatalDivZeroClass and fatalRangeClass are driven ONLY from a subprocess
	// (fatal_test.go): stock cannot be observed in-process on either.
	fatalDivZeroClass = "GLF_divzero"
	fatalRangeClass   = "GLF_hugerange"
)

// rowInstance is one row at one level: the unit the differential reports on.
type rowInstance struct {
	Row   guardRow
	Level level
	// Stock is the RECORDED stock envelope for this (row, level).
	Stock envelope
}

// label is the BAML attribute label, and the key a batched result is recovered
// by. Assert instances take a distinct label from their check sibling so a class
// carrying both cannot collide.
func (i rowInstance) label() string {
	if i.Level == levelAssert {
		return i.Row.ID + "_A"
	}
	return i.Row.ID
}

// attribute renders the BAML attribute for this instance.
func (i rowInstance) attribute() string {
	kind := "check"
	if i.Level == levelAssert {
		kind = "assert"
	}
	return fmt.Sprintf("@%s(%s, {{ %s }})", kind, i.label(), i.Row.Expr)
}

// batched reports whether this instance can ride in one of its group's batches.
//
// A @check that stock DECIDES is batchable either way: a failed check is DATA,
// so the node is still emitted and cannot take its neighbours down. A @assert is
// batchable only when it PASSES, because a failing one rejects the whole node.
// An erroring predicate rejects the node at either level and is always isolated.
//
// The two levels ride in SEPARATE classes ([checkBatchPrefix] /
// [assertBatchPrefix]) so a mis-pinned assert cannot make every check in the
// group unobservable — TestGuardLedgerBatchesParse then names exactly one batch.
func (i rowInstance) batched() bool {
	if i.Level == levelAssert {
		return i.Stock == envPass
	}
	return i.Stock == envPass || i.Stock == envFailedCheck
}

// excludedFromFixture reports whether the row cannot appear in the generated
// project at all, because BAML refuses to COMPILE its source spelling. Such a
// row is still carried by the corpus — dropping it would lose the observation —
// and is proved separately by TestGuardLedgerRejectedSourceSpellings.
func (i rowInstance) excludedFromFixture() bool {
	return i.Stock == envSourceRejected
}

// isoName is the class/function stem of an isolated instance.
func (i rowInstance) isoName() string {
	return isoPrefix + i.Row.ID + "_" + string(i.Level)
}

// method is the generated Parse client method carrying this instance.
func (i rowInstance) method() string {
	if !i.batched() {
		return i.isoName() + "Fn"
	}
	if i.Level == levelAssert {
		return assertBatchPrefix + i.Row.Group + "Fn"
	}
	return checkBatchPrefix + i.Row.Group + "Fn"
}

// rowInstances expands the corpus into the units the differential drives.
//
// A row always has a @check instance. It has a @assert instance exactly when
// StockAssert is set; AssertOmitted carries the reason otherwise, and
// TestGuardLedgerRowsAreWellFormed requires exactly one of the two.
func rowInstances() []rowInstance {
	out := make([]rowInstance, 0, len(guardRows)*2)
	for _, r := range guardRows {
		out = append(out, rowInstance{Row: r, Level: levelCheck, Stock: r.StockCheck})
		if r.StockAssert != "" {
			out = append(out, rowInstance{Row: r, Level: levelAssert, Stock: r.StockAssert})
		}
	}
	return out
}

// groupByName looks a group up, reporting an unknown one rather than defaulting.
func groupByName(name string) (guardGroup, bool) {
	for _, g := range guardGroups {
		if g.Name == name {
			return g, true
		}
	}
	return guardGroup{}, false
}

// rowByID looks a row up by its witness id.
func rowByID(id string) (guardRow, bool) {
	for _, r := range guardRows {
		if r.ID == id {
			return r, true
		}
	}
	return guardRow{}, false
}

// witnessIDs is every row id in the corpus, sorted — the set the ledger's
// WitnessRows column must be a subset of.
func witnessIDs() []string {
	out := make([]string, 0, len(guardRows))
	for _, r := range guardRows {
		out = append(out, r.ID)
	}
	sort.Strings(out)
	return out
}

// renderFixtureBAML renders baml_src/rows.baml from the corpus. It is the single
// source of truth for that file: TestGuardLedgerFixtureDrift byte-compares the
// two, and GUARD_LEDGER_FIXTURE_WRITE=1 rewrites it (after which the stock
// client must be regenerated — see the package comment).
func renderFixtureBAML() string {
	var b strings.Builder

	b.WriteString(`// GENERATED from internal/debaml/guardledger/corpus_test.go — do not edit.
//
// Regenerate with:
//   GUARD_LEDGER_FIXTURE_WRITE=1 go test -tags integration \
//     ./internal/debaml/guardledger -run TestGuardLedgerFixtureDrift
// then re-run the stock BAML v0.223.0 generator over this project (see the
// package comment).
//
// Every declaration here exists only to give the stock client a constrained
// node to evaluate. `)
	b.WriteString("The `v` field carries the @check/@assert attributes;\n")
	b.WriteString("// the attribute LABEL is the witness row id (suffixed `_A` at @assert\n")
	b.WriteString("// level), so a batched result is recoverable from the checks map by name.\n")
	b.WriteString(bamlPrelude)

	instances := rowInstances()

	for _, lv := range []level{levelCheck, levelAssert} {
		prefix := checkBatchPrefix
		if lv == levelAssert {
			prefix = assertBatchPrefix
		}
		for _, g := range guardGroups {
			var batched []rowInstance
			for _, i := range instances {
				if i.Row.Group == g.Name && i.Level == lv && i.batched() {
					batched = append(batched, i)
				}
			}
			if len(batched) == 0 {
				continue
			}
			name := prefix + g.Name
			fmt.Fprintf(&b, "\nclass %s {\n  v %s\n", name, g.BAMLType)
			for _, i := range batched {
				fmt.Fprintf(&b, "    %s\n", i.attribute())
			}
			b.WriteString("}\n")
			writeFixtureFunction(&b, name)
		}
	}

	for _, i := range instances {
		if i.batched() || i.excludedFromFixture() {
			continue
		}
		g, ok := groupByName(i.Row.Group)
		if !ok {
			// TestGuardLedgerRowsAreWellFormed rejects an unknown group first, so
			// reaching here means the fixture would silently omit a row — which
			// would present later as a missing Parse method rather than as this.
			panic("guardledger: row " + i.Row.ID + " references unknown group " + i.Row.Group)
		}
		name := i.isoName()
		fmt.Fprintf(&b, "\nclass %s {\n  v %s %s\n}\n", name, g.BAMLType, i.attribute())
		writeFixtureFunction(&b, name)
	}

	// Duplicate labels: two @check attributes under ONE label, which BAML's
	// checks map cannot hold twice. Recorded, not folded — see
	// TestGuardLedgerDuplicateLabelIsLastWriteWins.
	b.WriteString("\n// Driven ONLY by TestGuardLedgerDuplicateLabelIsLastWriteWins: two @check\n")
	b.WriteString("// attributes under one label, so the collapse is observed rather than assumed.\n")
	fmt.Fprintf(&b, "class %s {\n  v int\n    @check(dup, {{ this == 1 }})\n    @check(dup, {{ this == 2 }})\n}\n", dupClass)
	writeFixtureFunction(&b, dupClass)

	// The two rows stock cannot be asked in-process. Both are driven from an
	// isolated subprocess under a deadline — see fatal_test.go.
	b.WriteString("\n// Driven ONLY from a SUBPROCESS (fatal_test.go): stock BAML v0.223 aborts or\n")
	b.WriteString("// hangs its CFFI process on these, so they can never ride in this binary.\n")
	fmt.Fprintf(&b, "class %s {\n  v int @check(divzero, {{ this is divisibleby(0) }})\n}\n", fatalDivZeroClass)
	writeFixtureFunction(&b, fatalDivZeroClass)
	fmt.Fprintf(&b, "\nclass %s {\n  v int @check(hugerange, {{ range(1000000000000)|length == 1000000000000 }})\n}\n", fatalRangeClass)
	writeFixtureFunction(&b, fatalRangeClass)

	return b.String()
}

func writeFixtureFunction(b *strings.Builder, class string) {
	fmt.Fprintf(b, "\nfunction %sFn(topic: string) -> %s {\n  client %s\n  prompt #\"{{ topic }} {{ ctx.output_format }}\"#\n}\n",
		class, class, fixtureClient)
}
