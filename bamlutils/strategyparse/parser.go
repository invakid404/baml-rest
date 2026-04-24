// Package strategyparse extracts the shared token/list parsing logic used
// by both the fallback and round-robin strategy resolvers. The parsers
// were historically duplicated in bamlutils/buildrequest/orchestrator.go
// and bamlutils/buildrequest/roundrobin/resolver.go with subtle
// divergences (quote stripping in one, not the other) that caused a
// real bug under runtime client_registry overrides — see PR #192
// CodeRabbit round-robin cold review, finding 4.
//
// Placing the parser at bamlutils/strategyparse keeps it importable by
// both the root fallback package (bamlutils/buildrequest) and the leaf
// round-robin package (bamlutils/buildrequest/roundrobin) without an
// import cycle — it has no dependency on either.
package strategyparse

import (
	"strings"
)

// ParseStrategyOption extracts the ordered list of client names from the
// runtime client_registry strategy override value. Accepts three legal
// shapes that BAML's runtime ClientRegistry may expose:
//
//   - A bracket-delimited string ("strategy [A, B, C]" or "[A, B, C]").
//   - A native []string of pre-parsed client names.
//   - A []any of strings (typical of a JSON round-trip through map[string]any).
//
// Returns nil for unrecognised shapes or empty lists so callers can
// distinguish "no override" from "override with empty chain".
//
// Tokens are trimmed of surrounding whitespace and surrounding matched
// quote pairs. The quote stripping is load-bearing: BAML's runtime
// serialises a string-shaped strategy override with quotes around each
// element (operators typing `strategy ["ClientA", "ClientB"]` through a
// runtime override end up with the quotes intact in the string value),
// and downstream resolution matches on the unquoted introspected client
// name. A prior baml-rest revision split this behaviour between the
// fallback and round-robin paths; the round-robin path stopped stripping
// quotes and started failing provider lookup for quoted-string overrides.
// The resolvers now share this parser so both strategies behave the same.
func ParseStrategyOption(v any) []string {
	switch vv := v.(type) {
	case string:
		return parseBracketedString(vv)
	case []string:
		out := make([]string, 0, len(vv))
		for _, item := range vv {
			if s := normalizeToken(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(vv))
		for _, item := range vv {
			str, ok := item.(string)
			if !ok {
				// A heterogeneous list with a non-string element is
				// treated as invalid for a strategy override. The
				// caller's fallback map/resolver will take it from
				// here.
				return nil
			}
			if s := normalizeToken(str); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// parseBracketedString splits "strategy [A, B, C]" / "[A, B, C]" shapes
// into individual client-name tokens. Any leading "strategy " prefix is
// discarded and the inner content is then split on comma / whitespace /
// newline and each token normalised.
//
// BAML upstream rejects non-list strategy values — see
// baml-lib/llm-client/src/clients/helpers.rs ensure_array, which errors
// with "strategy must be an array. Got: <type>" on anything that isn't
// a vector/list. We mirror that contract by returning nil for any
// string that isn't explicitly bracketed after optional prefix
// stripping: bare tokens like "ClientA", prefix-only "strategy
// ClientA", or half-bracketed "strategy [ClientA" all return nil so
// callers fall back to the introspected chain instead of silently
// collapsing the strategy to a one-element override. The previous
// implementation accepted all of those shapes; that behaviour diverged
// from BAML upstream and silently changed client resolution on
// malformed runtime registry input.
func parseBracketedString(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "strategy ")
	s = strings.TrimSpace(s)
	// Require an explicit bracketed list after trim + optional
	// `strategy ` prefix stripping. len(s) >= 2 plus the prefix /
	// suffix check covers "[]" (empty list, legal) through to a
	// full bracketed list; anything else is rejected.
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil
	}
	s = s[1 : len(s)-1]
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if t := normalizeToken(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// normalizeToken trims whitespace and a single layer of matched
// surrounding quotes (single or double). Returns the empty string for
// empty input so callers can skip blanks without additional checks.
func normalizeToken(token string) string {
	token = strings.TrimSpace(token)
	if len(token) >= 2 {
		first, last := token[0], token[len(token)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			token = token[1 : len(token)-1]
		}
	}
	return strings.TrimSpace(token)
}
