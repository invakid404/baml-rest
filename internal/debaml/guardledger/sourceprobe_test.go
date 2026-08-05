//go:build integration

package guardledger

import (
	"fmt"
	"strings"
	"testing"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
)

// The STOCK COMPILER probe behind an [envSourceRejected] row.
//
// A row that records "BAML will not compile this spelling" is a claim about
// stock BAML v0.223, and it has to be answered by stock BAML v0.223. Asking the
// fork's parser instead would be fork-only evidence for a stock statement —
// exactly the substitution this whole slice exists to avoid — even though the
// two share a lineage and the guard in question stays kept either way.
//
// So the probe builds an ISOLATED in-memory project (the map[string]string
// shape the stock runtime takes) carrying the exact attribute bytes, and asks
// the stock CFFI to compile it. It is the same compiler the generated client
// runs at init, reached directly rather than through the CLI, so it needs no
// npx, no network and no second generated artifact.
//
// Every rejected row is driven with a CONTROL: the accepted alternative
// spelling, compiled through the same path. Without it the test would pass just
// as well against a project that is broken for some unrelated reason — a missing
// client, a bad generator block — and would prove nothing about the spelling.
//
// ONE LIMIT, STATED RATHER THAN PAPERED OVER. Stock's Go binding DISCARDS the
// compiler diagnostic: CreateBamlRuntime returns a bare
// `failed to create BAML runtime` (baml_go/exports.go:33), so the
// `Error parsing jinja template: syntax error: unexpected input after
// expression` the CLI prints is not reachable from here. What stock is
// authoritative for at this seam is therefore REJECT versus ACCEPT — which is
// the row's actual claim — and the probe proves exactly that, by swapping only
// the attribute bytes between two otherwise identical projects. The
// CLASSIFICATION of the rejection as a jinja syntax error is corroborated
// separately against the engine's parser, and is labelled as the weaker,
// fork-side evidence it is.

// probeProject renders a minimal, self-contained BAML project whose single
// class carries `attr` as its field attribute.
//
// It deliberately does NOT reuse the harness fixture: the point is an isolated
// project, so a compile failure can only come from the bytes under test.
func probeProject(attr string) map[string]string {
	return map[string]string{
		"probe.baml": fmt.Sprintf(`client<llm> ProbeClient {
  provider openai
  options {
    model "fake"
    api_key "fake"
    base_url "https://guard-ledger-probe.invalid/v1"
  }
}

class Probe {
  v string %s
}

function ProbeFn(topic: string) -> Probe {
  client ProbeClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
`, attr),
	}
}

// stockBuildFailureMessage is what stock's Go binding returns for ANY build
// failure (baml_go/exports.go:33). It carries no diagnostic — that is the limit
// stated in the package note — but pinning it still discriminates: it separates
// "the runtime refused to build this project" from any other error the call
// could surface, such as a changed binding, a panic turned into an error, or a
// failure to load the CFFI at all.
const stockBuildFailureMessage = "failed to create BAML runtime"

// compileWithStock asks the stock BAML v0.223 runtime to build the project,
// returning its error. A nil error means stock accepted the source.
func compileWithStock(files map[string]string) error {
	// No environment variables: the project names no env-backed option, and an
	// empty set keeps the probe independent of the developer's shell.
	_, err := baml.CreateRuntime("./baml_src", files, map[string]string{})
	return err
}

// TestGuardLedgerStockRejectsTheBareSubscriptSpelling is the authoritative
// source-row observation for [envSourceRejected], and its contract is exactly
// REJECT-VERSUS-ACCEPT — nothing about the diagnostic.
//
// For every such row it requires, from STOCK:
//
//   - the row's own spelling to be REJECTED; and
//   - the row's accepted alternative to COMPILE, through the same path, in an
//     otherwise byte-identical project.
//
// Those two together are what make the rejection attributable to the attribute
// bytes rather than to the construct or the scaffold, and they are the whole of
// what this test claims. Stock's Go binding does not expose the compiler
// diagnostic (see the package note above), so this test says nothing about the
// error's CLASS and must not be read as doing so; that weaker claim is
// corroborated separately, against the engine's parser, by
// TestGuardLedgerRejectedSourceSpellings.
func TestGuardLedgerStockRejectsTheBareSubscriptSpelling(t *testing.T) {
	// A sanity control first: the probe project itself must compile with a
	// predicate stock certainly accepts. Without this, a rejection below could
	// be an artefact of the scaffold rather than of the bytes under test.
	if err := compileWithStock(probeProject(`@check(ok, {{ this|length > 0 }})`)); err != nil {
		t.Fatalf("the probe scaffold does not compile with a trivially valid predicate, so it cannot "+
			"attribute a rejection to the spelling under test: %v", err)
	}

	seen := 0
	for _, r := range guardRows {
		if r.StockCheck != envSourceRejected {
			continue
		}
		seen++
		t.Run(r.ID, func(t *testing.T) {
			if r.AcceptedAlternative == "" {
				t.Fatalf("row %q records a rejected spelling but names no accepted alternative, so this probe "+
					"has no control to compare against", r.ID)
			}

			// THE SPELLING UNDER TEST. Note the row's Expr, not its retained
			// form: what is being compiled is the attribute source, and the
			// retained form is what BAML would report back only if it got that
			// far.
			//
			// Only the PRESENCE of an error is asserted. The binding collapses
			// every build failure to one opaque message, so an assertion about
			// WHICH failure it was would be a claim this test cannot make; the
			// acceptance control below is what rules out an unrelated cause.
			err := compileWithStock(probeProject(fmt.Sprintf("@check(%s, {{ %s }})", strings.ToLower(r.ID), r.Expr)))
			if err == nil {
				t.Fatalf("stock BAML v0.223 COMPILED %q, which the row records as a spelling it refuses; "+
					"the record is stale and the row should become an ordinary driveable one", r.Expr)
			}
			if !strings.Contains(err.Error(), stockBuildFailureMessage) {
				t.Fatalf("stock failed on %q with %q, which is not the runtime's build-failure error (%q). "+
					"The rejection this row records is a BUILD refusal; another failure mode would mean the "+
					"probe is measuring something else entirely.", r.Expr, err, stockBuildFailureMessage)
			}
			// THE CONTROL. The accepted alternative must compile through exactly
			// the same path, so the ONLY difference between a rejected and an
			// accepted project is the attribute bytes.
			if err := compileWithStock(probeProject(
				fmt.Sprintf("@check(%s, {{ %s }})", strings.ToLower(r.ID), r.AcceptedAlternative))); err != nil {
				t.Fatalf("the accepted alternative %q does not compile either, so the rejection above is about "+
					"the CONSTRUCT or the scaffold rather than the spelling:\n%v", r.AcceptedAlternative, err)
			}
			t.Logf("stock BAML v0.223 REJECTED %q and ACCEPTED %q through the same isolated project. "+
				"The rejection's diagnostic is not observable here — the binding reports only %q — so "+
				"reject-versus-accept is the whole of what this probe establishes.",
				r.Expr, r.AcceptedAlternative, err)
		})
	}
	if seen == 0 {
		t.Fatal("no row records a rejected source spelling; the unparenthesized subscript observation is missing")
	}
}
