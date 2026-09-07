package nativespine

import "strings"

// ListArgsFixtureBaseURL is the literal `base_url` the checked-in
// ListArgsFixtureSources carry. It is a placeholder: an oracle differential must
// point BOTH engines at the same loopback server, and the stock BAML side reads
// the client's base_url out of the .baml SOURCE (there is no descriptor to patch),
// so the substitution has to happen in the text. ListArgsFixtureSourcesAt does it
// for both legs from one call, which is what keeps them from drifting apart.
const ListArgsFixtureBaseURL = "http://127.0.0.1:0/v1"

// ListArgsFixtureSources is the representative .baml corpus for the required
// scalar-LIST input cohort: one admitted method whose return is the UNCHANGED exact
// five-arm `JSON` recursive alias and whose inputs mix the pre-existing required
// scalars — including a `float`, which stays admitted at the TOP level — with one
// required single-level list of each admitted ELEMENT primitive, so a differential
// over it exercises string, int and bool host-list rendering rather than
// generalising from strings.
//
// There is deliberately no `float[]`: stock v0.223 renders a float LIST ELEMENT with
// Rust's `Debug for f64` (exponent form from ~1e16 and ~1e-5) while the native list
// renderer is positional, a measured residual that keeps float out of the element
// set. See requiredScalarOrScalarListInputs in nativeserve/spine.
//
// The prompt interpolates every argument directly (`{{ tags }}`), which is the whole
// render surface this slice needs: BAML host-value rendering through the existing
// closed static grammar, with no loops, filters, or expressions. `ctx.output_format`
// is the same bare form the exact-JSON cohort already proves.
var ListArgsFixtureSources = map[string]string{
	"clients.baml": `client<llm> ListOracle {
  provider openai
  options {
    model "gpt-4o-mini"
    api_key "sk-scalarlist-not-a-real-secret"
    base_url "` + ListArgsFixtureBaseURL + `"
  }
}
`,
	"types.baml": `// The EXACT proven direct five-arm JSON recursive alias. UNCHANGED by this
// slice: only the INPUT cohort widens.
type JSON = int | string | bool | JSON[] | map<string, JSON>
`,
	"functions.baml": `// Admitted: static, five-arm JSON recursive-alias output, one literal-model
// OpenAI client, and inputs that mix the required scalars (string, int, float,
// bool) with one required single-level list of each admitted element primitive.
function StaticListArgsJSON(topic: string, ratio: float, tags: string[], counts: int[], flags: bool[]) -> JSON {
  client ListOracle
  prompt #"
    Summarize {{ topic }} as arbitrary JSON.
    Ratio: {{ ratio }}
    Tags: {{ tags }}
    Counts: {{ counts }}
    Flags: {{ flags }}
    {{ ctx.output_format }}
  "#
}
`,
}

// ListArgsFixtureMethod is the one admitted method in ListArgsFixtureSources.
const ListArgsFixtureMethod = "StaticListArgsJSON"

// ListArgsFixtureSourcesAt returns ListArgsFixtureSources with the placeholder
// base_url replaced by baseURL, so the native spine project and a stock BAML
// runtime compiled from the same text address the same server.
func ListArgsFixtureSourcesAt(baseURL string) map[string]string {
	out := make(map[string]string, len(ListArgsFixtureSources))
	for name, src := range ListArgsFixtureSources {
		out[name] = strings.ReplaceAll(src, ListArgsFixtureBaseURL, baseURL)
	}
	return out
}
