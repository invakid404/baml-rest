package nativeschema

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// parseClientConfigs parses one inline .baml source and runs BuildClientConfigs.
func parseClientConfigs(t *testing.T, src string) map[string]promptdescriptor.ClientConfig {
	t.Helper()
	f, err := bamlparser.ParseString("clientconfig_test.baml", src)
	if err != nil {
		t.Fatalf("ParseString: %v", err)
	}
	return BuildClientConfigs([]SourceFile{{File: f, Path: "clientconfig_test.baml"}})
}

// TestBuildClientConfigsOrderedRequestBody proves the ordered, typed request_body
// retention with last-wins placement, nested object/list values, the
// transport-only vs body-affecting split, and literal model provenance.
func TestBuildClientConfigsOrderedRequestBody(t *testing.T) {
	src := `
client<llm> RichClient {
  provider openai
  options {
    model "gpt-4o"
    api_key "sk-x"
    base_url "https://api.example.com/v1"
    temperature 0.7
    request_body {
      top_p 0.9
      seed 42
      nested {
        a "x"
        b 1
      }
      stops ["a" "b"]
      seed 7
      flag true
    }
  }
}
`
	cfgs := parseClientConfigs(t, src)
	cfg, ok := cfgs["RichClient"]
	if !ok {
		t.Fatalf("no ClientConfig for RichClient; got %v", keysOfConfigs(cfgs))
	}
	if !cfg.Present {
		t.Fatalf("RichClient config must be Present")
	}

	// Model provenance: a string literal.
	if cfg.Model.Provenance != promptdescriptor.ModelProvenanceLiteral || cfg.Model.Value != "gpt-4o" {
		t.Errorf("model = %+v, want literal gpt-4o", cfg.Model)
	}

	// Transport-only options in source order (api_key, base_url); model/request_body
	// excluded; temperature is NOT transport-only.
	wantTransport := []string{"api_key", "base_url"}
	if got := optionKeys(cfg.TransportOptions); !equalStrings(got, wantTransport) {
		t.Errorf("transport options = %v, want %v", got, wantTransport)
	}

	// Body-affecting (non-request_body) options: temperature only.
	wantBody := []string{"temperature"}
	if got := optionKeys(cfg.BodyAffectingOptions); !equalStrings(got, wantBody) {
		t.Errorf("body-affecting options = %v, want %v", got, wantBody)
	}

	// request_body: last-wins on duplicate `seed` (survivor keeps its LAST
	// position with the LAST value), preserving source order otherwise.
	wantKeys := []string{"top_p", "nested", "stops", "seed", "flag"}
	if got := entryKeys(cfg.RequestBody); !equalStrings(got, wantKeys) {
		t.Fatalf("request_body key order = %v, want %v (last-wins on seed)", got, wantKeys)
	}
	byKey := indexEntries(cfg.RequestBody)

	// top_p: number.
	if v := byKey["top_p"]; v.Kind != promptdescriptor.OptionNumber || v.Number != "0.9" {
		t.Errorf("top_p = %+v, want number 0.9", v)
	}
	// seed: last value wins (7, not 42).
	if v := byKey["seed"]; v.Kind != promptdescriptor.OptionNumber || v.Number != "7" {
		t.Errorf("seed = %+v, want number 7 (last-wins)", v)
	}
	// flag: bool true.
	if v := byKey["flag"]; v.Kind != promptdescriptor.OptionBool || v.Bool != true {
		t.Errorf("flag = %+v, want bool true", v)
	}
	// nested: object with ordered a (string), b (number).
	nested := byKey["nested"]
	if nested.Kind != promptdescriptor.OptionObject {
		t.Fatalf("nested = %+v, want object", nested)
	}
	if got := entryKeys(nested.Object); !equalStrings(got, []string{"a", "b"}) {
		t.Errorf("nested keys = %v, want [a b]", got)
	}
	nb := indexEntries(nested.Object)
	if nb["a"].Kind != promptdescriptor.OptionString || nb["a"].String != "x" {
		t.Errorf("nested.a = %+v, want string x", nb["a"])
	}
	if nb["b"].Kind != promptdescriptor.OptionNumber || nb["b"].Number != "1" {
		t.Errorf("nested.b = %+v, want number 1", nb["b"])
	}
	// stops: list of two strings.
	stops := byKey["stops"]
	if stops.Kind != promptdescriptor.OptionList || len(stops.List) != 2 {
		t.Fatalf("stops = %+v, want list of 2", stops)
	}
	if stops.List[0].Kind != promptdescriptor.OptionString || stops.List[0].String != "a" ||
		stops.List[1].String != "b" {
		t.Errorf("stops = %+v, want [a b]", stops.List)
	}
}

// TestBuildClientConfigsModelProvenance covers env / dynamic / absent model
// provenance and the empty-request_body case.
func TestBuildClientConfigsModelProvenance(t *testing.T) {
	src := `
client<llm> EnvModel {
  provider openai
  options {
    model env.OPENAI_MODEL
  }
}

client<llm> DynamicModel {
  provider openai
  options {
    model some_ident
  }
}

client<llm> NoModel {
  provider openai
  options {
    api_key "k"
  }
}
`
	cfgs := parseClientConfigs(t, src)

	env := cfgs["EnvModel"]
	if env.Model.Provenance != promptdescriptor.ModelProvenanceEnv || env.Model.EnvVar != "OPENAI_MODEL" {
		t.Errorf("EnvModel model = %+v, want env OPENAI_MODEL", env.Model)
	}
	if env.Model.Value != "" {
		t.Errorf("env model must have no literal value, got %q", env.Model.Value)
	}
	// EnvModel declares NO request_body block.
	if env.RequestBodyPresent {
		t.Errorf("EnvModel declares no request_body; RequestBodyPresent must be false")
	}

	dyn := cfgs["DynamicModel"]
	if dyn.Model.Provenance != promptdescriptor.ModelProvenanceDynamic || dyn.Model.Value != "" {
		t.Errorf("DynamicModel model = %+v, want dynamic (no literal)", dyn.Model)
	}

	none := cfgs["NoModel"]
	if none.Model.Provenance != promptdescriptor.ModelProvenanceAbsent {
		t.Errorf("NoModel model = %+v, want absent", none.Model)
	}
	if got := optionKeys(none.TransportOptions); !equalStrings(got, []string{"api_key"}) {
		t.Errorf("NoModel transport = %v, want [api_key]", got)
	}
}

// TestBuildClientConfigsRequestBodyPresence establishes the load-bearing
// empty-vs-absent boundary: an explicit `request_body {}` block is PRESENT with
// zero entries (BAML v0.223 emits "request_body":{} for it), which is distinct
// from a client that declares no request_body block at all.
func TestBuildClientConfigsRequestBodyPresence(t *testing.T) {
	src := `
client<llm> EmptyBlock {
  provider openai
  options {
    model "m"
    request_body {}
  }
}

client<llm> AbsentBlock {
  provider openai
  options {
    model "m"
  }
}

client<llm> NonEmptyBlock {
  provider openai
  options {
    model "m"
    request_body { top_p 0.9 }
  }
}
`
	cfgs := parseClientConfigs(t, src)

	empty := cfgs["EmptyBlock"]
	if !empty.RequestBodyPresent {
		t.Errorf("EmptyBlock: explicit request_body {} must set RequestBodyPresent=true")
	}
	if len(empty.RequestBody) != 0 {
		t.Errorf("EmptyBlock: request_body {} must have zero entries, got %v", empty.RequestBody)
	}

	absent := cfgs["AbsentBlock"]
	if absent.RequestBodyPresent {
		t.Errorf("AbsentBlock: no request_body block must set RequestBodyPresent=false")
	}
	if len(absent.RequestBody) != 0 {
		t.Errorf("AbsentBlock: no request_body must have zero entries, got %v", absent.RequestBody)
	}

	nonEmpty := cfgs["NonEmptyBlock"]
	if !nonEmpty.RequestBodyPresent || len(nonEmpty.RequestBody) != 1 || nonEmpty.RequestBody[0].Key != "top_p" {
		t.Errorf("NonEmptyBlock: want present with [top_p], got present=%v %v", nonEmpty.RequestBodyPresent, nonEmpty.RequestBody)
	}
}

// TestBuildClientConfigsModelRawString proves the extractor distinguishes a
// REGULAR string model literal (RawString==false; BAML decodes escapes) from a
// RAW-string literal (RawString==true; BAML keeps it verbatim), retaining the
// bytes without escape processing in both cases.
func TestBuildClientConfigsModelRawString(t *testing.T) {
	src := "" +
		"client<llm> RegularModel {\n" +
		"  provider openai\n" +
		"  options { model \"gpt\\t4\" }\n" +
		"}\n" +
		"client<llm> RawModel {\n" +
		"  provider openai\n" +
		"  options { model #\"gpt\\t4\"# }\n" +
		"}\n"
	cfgs := parseClientConfigs(t, src)

	reg := cfgs["RegularModel"].Model
	if reg.Provenance != promptdescriptor.ModelProvenanceLiteral || reg.RawString {
		t.Errorf("RegularModel: want literal RawString=false, got %+v", reg)
	}
	if reg.Value != `gpt\t4` {
		t.Errorf("RegularModel: escape bytes must be retained verbatim, got %q", reg.Value)
	}

	raw := cfgs["RawModel"].Model
	if raw.Provenance != promptdescriptor.ModelProvenanceLiteral || !raw.RawString {
		t.Errorf("RawModel: want literal RawString=true, got %+v", raw)
	}
	if raw.Value != `gpt\t4` {
		t.Errorf("RawModel: value = %q, want gpt\\t4", raw.Value)
	}
}

// TestBuildClientConfigsDuplicateClientLastWins proves duplicate client names
// resolve last-wins, matching BAML's client map.
func TestBuildClientConfigsDuplicateClientLastWins(t *testing.T) {
	src := `
client<llm> Dup {
  provider openai
  options { model "first" }
}
client<llm> Dup {
  provider openai
  options { model "second" }
}
`
	cfgs := parseClientConfigs(t, src)
	if cfgs["Dup"].Model.Value != "second" {
		t.Errorf("duplicate client last-wins failed: model = %q, want second", cfgs["Dup"].Model.Value)
	}
}

// --- small local helpers ---

func keysOfConfigs(m map[string]promptdescriptor.ClientConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func optionKeys(opts []promptdescriptor.ClientOption) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.Key)
	}
	return out
}

func entryKeys(entries []promptdescriptor.RequestBodyEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Key)
	}
	return out
}

func indexEntries(entries []promptdescriptor.RequestBodyEntry) map[string]promptdescriptor.OptionValue {
	out := make(map[string]promptdescriptor.OptionValue, len(entries))
	for _, e := range entries {
		out[e.Key] = e.Value
	}
	return out
}
