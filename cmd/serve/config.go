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
// at server startup. Truthy parsing intentionally matches the
// parseBuildRequestEnv contract over in bamlutils/buildrequest so the
// two env vars behave identically — 1/true/yes/on (case-insensitive)
// enable the default; every other value disables it.
func preserveSchemaOrderDefaultFromEnv() bool {
	return parseTruthyEnvBool(os.Getenv(envPreserveSchemaOrderDefault))
}

// parseTruthyEnvBool mirrors parseBuildRequestEnv: 1/true/yes/on after
// lowercasing count as truthy, anything else (including empty string,
// whitespace-padded variants, and unrecognized tokens) is falsy.
func parseTruthyEnvBool(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
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
