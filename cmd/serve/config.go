package main

import (
	"os"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils"
)

// envPreserveSchemaOrderDefault names the server-level default switch for
// the dynamic-endpoint preserve_schema_order opt-in. When truthy, dynamic
// requests that omit or null preserve_schema_order inherit true.
const envPreserveSchemaOrderDefault = "BAML_REST_PRESERVE_SCHEMA_ORDER_DEFAULT"

// preserveSchemaOrderDefaultFromEnv resolves BAML_REST_PRESERVE_SCHEMA_ORDER_DEFAULT
// at server startup. Truthy parsing intentionally matches the shared
// bamlutils.IsTruthyEnvValue contract so baml-rest's boolean env vars
// behave identically — 1/true/yes/on (case-insensitive) enable the
// default; every other value disables it.
func preserveSchemaOrderDefaultFromEnv() bool {
	return parseTruthyEnvBool(os.Getenv(envPreserveSchemaOrderDefault))
}

// parseTruthyEnvBool mirrors bamlutils.IsTruthyEnvValue: 1/true/yes/on
// after lowercasing count as truthy, anything else (including empty
// string, whitespace-padded variants, and unrecognized tokens) is falsy.
func parseTruthyEnvBool(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// retiredEnvVar names a retired BAML_REST_* environment variable and the
// operator-facing message emitted when it is still present. Retired vars
// are warn-and-ignore: any value (truthy or not) triggers the warning and
// none affect routing.
type retiredEnvVar struct {
	name string
	msg  string
}

// retiredEnvVars is the host-side warn-and-ignore list. cmd/serve emits one
// warning per present entry at startup. This host startup path runs in both
// in-process and subprocess modes, so it is the single emission point — the
// pooled worker subprocess deliberately stays silent to avoid one warning
// per worker.
var retiredEnvVars = []retiredEnvVar{
	{
		name: "BAML_REST_USE_BUILD_REQUEST",
		msg: "BAML_REST_USE_BUILD_REQUEST is retired and ignored: the BuildRequest route is " +
			"attempted whenever the generated BAML client exposes Request or StreamRequest. " +
			"Remove the variable from your configuration.",
	},
	{
		name: "BAML_REST_DISABLE_CALL_BUILD_REQUEST",
		msg: "BAML_REST_DISABLE_CALL_BUILD_REQUEST is retired and ignored: /call uses the " +
			"non-streaming Request API whenever the generated BAML client exposes Request and " +
			"the provider supports it; StreamRequest remains the automatic fallback when " +
			"Request is unavailable. Remove the variable from your configuration.",
	},
}

// presentRetiredEnvWarnings returns the warning messages for every retired
// env var currently present, in declaration order. Presence — not
// truthiness — gates the warning: any value (including empty) warns,
// matching the warn-and-ignore contract. Factored out so the retirement
// contract is unit-testable without capturing log output. lookup is
// os.LookupEnv in production.
func presentRetiredEnvWarnings(lookup func(string) (string, bool)) []string {
	var out []string
	for _, v := range retiredEnvVars {
		if _, present := lookup(v.name); present {
			out = append(out, v.msg)
		}
	}
	return out
}

// applyPreserveSchemaOrderDefault fills in the request-level *bool
// tri-state from the server default when the caller omitted the field
// (or sent JSON null, which decodes to nil). A non-nil pointer is left
// untouched so per-request true/false always wins over the server
// default. Must be called after json.Unmarshal and before ToWorkerInput
// so the resolved value drives dynamic_types.preserve_order downstream.
func applyPreserveSchemaOrderDefault(in *bamlutils.DynamicInput, defaultValue bool) {
	if in != nil && in.PreserveSchemaOrder == nil {
		in.PreserveSchemaOrder = &defaultValue
	}
}

// applyParsePreserveSchemaOrderDefault mirrors applyPreserveSchemaOrderDefault
// for the parse-input request type.
func applyParsePreserveSchemaOrderDefault(in *bamlutils.DynamicParseInput, defaultValue bool) {
	if in != nil && in.PreserveSchemaOrder == nil {
		in.PreserveSchemaOrder = &defaultValue
	}
}
