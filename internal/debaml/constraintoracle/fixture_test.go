//go:build integration

package constraintoracle

import (
	"fmt"
	"strings"
)

// Fixture layout.
//
// The stock leg needs one generated BAML function per DRIVEABLE unit, and a
// constraint whose predicate raises an evaluation error fails the WHOLE node —
// so a batch containing one such expression would lose every other result in
// that batch. The corpus is therefore split two ways:
//
//   - a GROUP BATCH per `this` value, carrying every case whose stock outcome is
//     a real check status. One parse yields all of their statuses at once,
//     keyed by the case label; and
//   - one ISOLATED function per case whose stock outcome is an error (or the
//     optional-swallow no-checks case), so its failure cannot take anything
//     else down with it.
//
// 19 batches + 25 isolated functions, versus 310 functions if every case had
// its own — which mattered: the one-function-per-case discovery project
// generated a 3.7 MB client, this fixture generates well under a tenth of that.

const (
	// fixtureClient is the client every fixture function names. It is never
	// called — the oracle drives Parse.<Method>, not the LLM — but a BAML
	// function must declare one.
	fixtureClient = "ConstraintOracleClient"

	batchPrefix = "Batch_"
	isoPrefix   = "Iso_"
)

// isolated reports whether a case gets its own BAML function rather than riding
// in its group's batch: it does exactly when stock does not produce a check
// status for it.
func (c constraintCase) isolated() bool {
	return c.Stock == outError || c.Stock == outNoChecks
}

// retainedExpr is the JinjaExpression BAML evaluates for this case — what the
// NATIVE leg must be given, so the differential compares two engines over the
// same source rather than comparing BAML's attribute lexer against Go's.
func (c constraintCase) retainedExpr() string {
	if c.Retained != "" {
		return c.Retained
	}
	return c.Expr
}

// bamlMethod is the generated Parse client method name for the function that
// carries this case.
func (c constraintCase) bamlMethod() string {
	if c.isolated() {
		return isoPrefix + c.Label + "Fn"
	}
	return batchPrefix + c.Group + "Fn"
}

// groupByName looks a group up by name, failing loudly on an unknown one (the
// corpus is checked for this by the drift test).
func groupByName(name string) (constraintGroup, bool) {
	for _, g := range constraintGroups {
		if g.Name == name {
			return g, true
		}
	}
	return constraintGroup{}, false
}

// renderFixtureBAML renders the checked-in baml_src/constraints.baml from the
// corpus. It is the single source of truth for that file: TestConstraintFixtureDrift
// byte-compares the two, and setting BAML_CONSTRAINT_FIXTURE_WRITE=1 rewrites it
// (after which the stock client must be regenerated — see the package comment).
func renderFixtureBAML() string {
	var b strings.Builder

	b.WriteString(`// GENERATED from internal/debaml/constraintoracle/corpus_test.go — do not edit.
//
// Regenerate with:
//   BAML_CONSTRAINT_FIXTURE_WRITE=1 go test -tags integration \
//     ./internal/debaml/constraintoracle -run TestConstraintFixtureDrift
// then re-run the stock generator over this project (see the package comment).
//
// Every function here exists only to give the stock BAML v0.223.0 client a
// constrained node to evaluate. `)
	b.WriteString("The `v` field carries the @check(s); the\n")
	b.WriteString("// check LABEL is the corpus case label, so a batched result is recoverable\n")
	b.WriteString("// from the checks map by name.\n")
	b.WriteString(bamlPrelude)
	b.WriteString("\n")

	for _, g := range constraintGroups {
		var batched []constraintCase
		for _, c := range constraintCases {
			if c.Group == g.Name && !c.isolated() {
				batched = append(batched, c)
			}
		}
		if len(batched) == 0 {
			continue
		}
		name := batchPrefix + g.Name
		fmt.Fprintf(&b, "\nclass %s {\n  v %s\n", name, g.BAMLType)
		for _, c := range batched {
			fmt.Fprintf(&b, "    @check(%s, {{ %s }})\n", c.Label, c.Expr)
		}
		b.WriteString("}\n")
		writeFixtureFunction(&b, name)
	}

	for _, c := range constraintCases {
		if !c.isolated() {
			continue
		}
		g, ok := groupByName(c.Group)
		if !ok {
			// Unreachable: the drift test rejects an unknown group first.
			continue
		}
		name := isoPrefix + c.Label
		fmt.Fprintf(&b, "\nclass %s {\n  v %s @check(%s, {{ %s }})\n}\n",
			name, g.BAMLType, c.Label, c.Expr)
		writeFixtureFunction(&b, name)
	}

	return b.String()
}

func writeFixtureFunction(b *strings.Builder, class string) {
	fmt.Fprintf(b, "\nfunction %sFn(topic: string) -> %s {\n  client %s\n  prompt #\"{{ topic }} {{ ctx.output_format }}\"#\n}\n",
		class, class, fixtureClient)
}
