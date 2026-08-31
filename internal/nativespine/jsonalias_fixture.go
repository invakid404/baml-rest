package nativespine

// JSONAliasFixtureSources is the representative .baml corpus for the ExecBridge-U1
// real-population vertical: the single admitted static-unary method whose return is
// the EXACT proven direct five-arm `JSON` recursive alias
// (int | string | bool | JSON[] | map<string, JSON>) with one required `string`
// scalar argument and one literal-model OpenAI client. It is the anchor the scope
// names (StaticRecursiveAliasJSON(topic) -> JSON) reduced to a self-contained
// corpus, so the fixture package can be regenerated hermetically without the heavy
// baml_client static_oracle testdata.
//
// The client carries literal base_url/api_key placeholders so the descriptor is
// SERVE-shaped (staticTransport wants literal transport options); the real-population
// integration test overrides base_url with its loopback server before constructing
// the runtime, exactly as the existing static-serve integration test injects its
// loopback URL through AdmitStaticClaimForTest.
var JSONAliasFixtureSources = map[string]string{
	"clients.baml": `client<llm> JSONOracle {
  provider openai
  options {
    model "gpt-4o-mini"
    api_key "sk-execbridge-u1-not-a-real-secret"
    base_url "http://127.0.0.1:0/v1"
  }
}
`,
	"types.baml": `// The EXACT proven direct five-arm JSON recursive alias (ExecBridge-U1 cohort).
type JSON = int | string | bool | JSON[] | map<string, JSON>
`,
	"functions.baml": `// Admitted: static unary, five-arm JSON recursive-alias output, one required
// string scalar input, one literal-model OpenAI client.
function StaticRecursiveAliasJSON(topic: string) -> JSON {
  client JSONOracle
  prompt #"Return a JSON document describing {{ topic }}."#
}
`,
}
