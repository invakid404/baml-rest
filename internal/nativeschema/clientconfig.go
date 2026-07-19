package nativeschema

// clientconfig.go is the de-BAML Phase 4a passive client/options extractor. It
// walks the parsed native AST and, for every declared `client<llm>` block,
// produces a promptdescriptor.ClientConfig: the resolved literal model (with
// provenance), the ordered typed request_body tree, and the recognized
// transport-only vs body-affecting option split.
//
// It is a sibling of the prompt-descriptor builder (prompt.go) and, like it, is
// PASSIVE: it lowers no values, resolves no env, and serializes nothing itself.
// BuildPromptDescriptors stamps the result onto each eligible function's
// descriptor, which cmd/introspect EMITS into the generated introspected package
// as of de-BAML Phase 8A (#602) — so a ClientConfig's inline option literals
// become sensitive representation-only generated-source + binary material (see
// promptdescriptor's package doc), though this extractor reaches no request path.
// The static differential oracle and cmd/introspect both call BuildClientConfigs
// on the same parsed files, so the production build path and the oracle preserve
// identical option data.
//
// Ordering and duplicate resolution mirror BAML: options are kept in source
// order and duplicate keys resolve last-wins, with the surviving key keeping the
// position of its LAST declaration (BAML builds its option map later-wins).

import (
	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// transportOnlyClientOptions is the recognized set of transport-only client
// options for the openai-family providers: HTTP/credential values that never
// enter the chat body. It is intentionally minimal and explicit — every option
// key NOT in this set (and not `model`/`request_body`) is classified
// body-affecting, so a later native body builder fails closed on anything whose
// body effect has not been individually proven.
var transportOnlyClientOptions = map[string]bool{
	"api_key":  true,
	"base_url": true,
	"headers":  true,
}

// BuildClientConfigs extracts a passive [promptdescriptor.ClientConfig] for
// every declared `client<llm>` block across the parsed files, keyed by client
// name. Duplicate client names resolve last-wins (matching BAML's client map and
// cmd/introspect's stale-state cleanup). Clients with no declared block (a
// shorthand spec, or an enriched-only name) simply have no entry; the descriptor
// builder then stamps a Present==false ClientConfig for them.
//
// The returned Provider on each config is the block's own raw provider value;
// BuildPromptDescriptors overrides it with the canonical resolved provider so a
// descriptor's ClientConfig.Provider always equals its Function.Provider.
func BuildClientConfigs(files []SourceFile) map[string]promptdescriptor.ClientConfig {
	out := make(map[string]promptdescriptor.ClientConfig)
	for _, sf := range files {
		if sf.File == nil {
			continue
		}
		for _, it := range sf.File.Items {
			c := it.Client
			if c == nil || c.Name == "" {
				continue
			}
			// Only `client<llm>` blocks (the LLM client shape) — matching
			// cmd/introspect's processBAMLFile dispatch, which accepts an explicit
			// "llm" type param or the bare (empty) form.
			if c.TypeParam != "llm" && c.TypeParam != "" {
				continue
			}
			out[c.Name] = buildOneClientConfig(c)
		}
	}
	return out
}

// buildOneClientConfig extracts one client block's body-relevant configuration.
// Provider (last-wins) is recorded for standalone use; model, request_body, and
// the transport/body-affecting option split come from the `options` block(s) in
// source order.
func buildOneClientConfig(c *bamlparser.ClientBlock) promptdescriptor.ClientConfig {
	cfg := promptdescriptor.ClientConfig{Present: true, Name: c.Name}
	for _, f := range c.Fields {
		switch f.Key {
		case "provider":
			if f.Value != nil {
				if s, ok := scalarOptionString(f.Value); ok {
					cfg.Provider = s
				}
			}
		case "options":
			if f.Block != nil {
				applyOptionsBlock(&cfg, f.Block)
			}
		}
	}
	return cfg
}

// applyOptionsBlock walks one options block in source order, routing each field
// to the model slot, the request_body tree, or the transport/body-affecting
// option buckets. Later declarations win.
func applyOptionsBlock(cfg *promptdescriptor.ClientConfig, opts *bamlparser.Block) {
	for _, f := range opts.Fields {
		switch {
		case f.Key == "model":
			cfg.Model = resolveClientModel(f.Value)
		case f.Key == "request_body" && f.Block != nil:
			// request_body is a special block BAML v0.223 emits as a
			// "request_body":{...} object (empty or not) after model. Its PRESENCE
			// is load-bearing — even `request_body {}` makes BAML emit the object —
			// so record RequestBodyPresent regardless of emptiness. A later
			// whole-block declaration replaces an earlier one (last-wins); duplicate
			// keys WITHIN the block are resolved by buildObjectEntries.
			cfg.RequestBodyPresent = true
			cfg.RequestBody = buildObjectEntries(f.Block)
		case transportOnlyClientOptions[f.Key]:
			cfg.TransportOptions = upsertClientOption(cfg.TransportOptions, f.Key, optionValueOfField(f))
		default:
			// Everything else — an explicit `request_body` given as a bare value,
			// or any unrecognized option (e.g. temperature) — is body-affecting:
			// BAML flattens it into the JSON body. Recorded so a native builder
			// declines until it is individually proven.
			cfg.BodyAffectingOptions = upsertClientOption(cfg.BodyAffectingOptions, f.Key, optionValueOfField(f))
		}
	}
}

// resolveClientModel resolves an `options { model ... }` value into a
// [promptdescriptor.ClientModel] with provenance. Only a string/raw literal
// yields a build-time literal; env references and bare identifiers/numbers are
// recorded with their provenance but no usable literal.
func resolveClientModel(v *bamlparser.Value) promptdescriptor.ClientModel {
	if v == nil {
		return promptdescriptor.ClientModel{Provenance: promptdescriptor.ModelProvenanceAbsent}
	}
	if s, ok := v.LiteralValue(); ok {
		// Regular string literal: BAML DECODES escapes before use, so the retained
		// (escape-preserving) value is only byte-safe when it has no escapes.
		return promptdescriptor.ClientModel{Value: s, Provenance: promptdescriptor.ModelProvenanceLiteral, RawString: false}
	}
	if s, ok := v.RawValue(); ok {
		// Raw string literal (`#"..."#`): verbatim in BAML, so the retained value is
		// byte-exact as-is.
		return promptdescriptor.ClientModel{Value: s, Provenance: promptdescriptor.ModelProvenanceLiteral, RawString: true}
	}
	if name, ok := v.EnvName(); ok {
		return promptdescriptor.ClientModel{Provenance: promptdescriptor.ModelProvenanceEnv, EnvVar: name}
	}
	// A bare identifier, number, or list is not a literal model string we can
	// claim byte-parity for at build time.
	return promptdescriptor.ClientModel{Provenance: promptdescriptor.ModelProvenanceDynamic}
}

// buildObjectEntries lowers a brace-delimited block into ordered
// [promptdescriptor.RequestBodyEntry] values, resolving duplicate keys last-wins
// (the surviving entry keeps the position of its last declaration).
func buildObjectEntries(b *bamlparser.Block) []promptdescriptor.RequestBodyEntry {
	var out []promptdescriptor.RequestBodyEntry
	for _, f := range b.Fields {
		out = removeEntry(out, f.Key)
		out = append(out, promptdescriptor.RequestBodyEntry{Key: f.Key, Value: optionValueOfField(f)})
	}
	return out
}

// optionValueOfField lowers one field's right-hand side (a nested block or a
// scalar/list value) into a typed [promptdescriptor.OptionValue]. A field with
// neither a value nor a block yields the zero (absent) OptionValue.
func optionValueOfField(f *bamlparser.Field) promptdescriptor.OptionValue {
	if f.Block != nil {
		return promptdescriptor.OptionValue{Kind: promptdescriptor.OptionObject, Object: buildObjectEntries(f.Block)}
	}
	if f.Value != nil {
		return valueToOption(f.Value)
	}
	return promptdescriptor.OptionValue{}
}

// valueToOption lowers a parsed [bamlparser.Value] into a typed OptionValue,
// preserving its source shape (string/number/bool/ident/env/list) so a later
// phase can serialize it exactly as BAML would.
func valueToOption(v *bamlparser.Value) promptdescriptor.OptionValue {
	if s, ok := v.LiteralValue(); ok {
		return promptdescriptor.OptionValue{Kind: promptdescriptor.OptionString, String: s}
	}
	if s, ok := v.RawValue(); ok {
		return promptdescriptor.OptionValue{Kind: promptdescriptor.OptionString, String: s}
	}
	if name, ok := v.EnvName(); ok {
		return promptdescriptor.OptionValue{Kind: promptdescriptor.OptionEnv, String: name}
	}
	if s, ok := v.NumberValue(); ok {
		return promptdescriptor.OptionValue{Kind: promptdescriptor.OptionNumber, Number: s}
	}
	if s, ok := v.IdentValue(); ok {
		switch s {
		case "true":
			return promptdescriptor.OptionValue{Kind: promptdescriptor.OptionBool, Bool: true}
		case "false":
			return promptdescriptor.OptionValue{Kind: promptdescriptor.OptionBool, Bool: false}
		default:
			return promptdescriptor.OptionValue{Kind: promptdescriptor.OptionIdent, String: s}
		}
	}
	if v.IsList() {
		elems := make([]promptdescriptor.OptionValue, 0, len(v.List))
		for _, e := range v.List {
			if e == nil {
				continue
			}
			elems = append(elems, valueToOption(e))
		}
		return promptdescriptor.OptionValue{Kind: promptdescriptor.OptionList, List: elems}
	}
	return promptdescriptor.OptionValue{}
}

// upsertClientOption appends key/val to opts with last-wins semantics: any prior
// entry for key is removed and the new one appended, so the surviving entry
// keeps the position of its LAST declaration.
func upsertClientOption(opts []promptdescriptor.ClientOption, key string, val promptdescriptor.OptionValue) []promptdescriptor.ClientOption {
	filtered := opts[:0:0]
	for _, o := range opts {
		if o.Key != key {
			filtered = append(filtered, o)
		}
	}
	return append(filtered, promptdescriptor.ClientOption{Key: key, Value: val})
}

// removeEntry drops any entry for key from a RequestBodyEntry slice, preserving
// the order of the rest (last-wins helper for buildObjectEntries).
func removeEntry(entries []promptdescriptor.RequestBodyEntry, key string) []promptdescriptor.RequestBodyEntry {
	filtered := entries[:0:0]
	for _, e := range entries {
		if e.Key != key {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// scalarOptionString extracts a client field value as a scalar string, mirroring
// the client-name/provider scalar conventions used elsewhere in the introspect
// pipeline. Used only for the standalone Provider record.
func scalarOptionString(v *bamlparser.Value) (string, bool) {
	if s, ok := v.LiteralValue(); ok {
		return s, true
	}
	if s, ok := v.IdentValue(); ok {
		return s, true
	}
	if s, ok := v.RawValue(); ok {
		return s, true
	}
	if s, ok := v.NumberValue(); ok {
		return s, true
	}
	if name, ok := v.EnvName(); ok {
		return "env." + name, true
	}
	return "", false
}
