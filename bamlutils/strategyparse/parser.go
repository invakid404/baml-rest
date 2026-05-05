// Package strategyparse extracts the shared token/list parsing logic used
// by both the fallback and round-robin strategy resolvers. Both
// callers (bamlutils/buildrequest/orchestrator.go and
// bamlutils/buildrequest/roundrobin/resolver.go) share this single
// implementation so they can never diverge on quote handling,
// empty-list semantics, or bracketed-string parsing.
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
// Returns nil for unrecognised shapes AND for empty lists, matching
// BAML upstream's ensure_strategy check in
// baml-lib/llm-client/src/clients/helpers.rs (which rejects empty
// arrays with "strategy must not be empty").
//
// Parser-level nil is a presence-blind signal: it means "this value
// did not parse to a non-empty chain" and nothing more. The parser
// cannot distinguish "no `strategy` key was supplied" from "the
// `strategy` key is present but malformed". That three-state
// classification is the caller's job — see InspectStrategyOverride
// (bamlutils/buildrequest/roundrobin/resolver.go) which feeds:
//
//   - absent          → caller uses the introspected chain
//   - valid override  → caller honours the parsed chain
//   - present-but-
//     invalid         → caller surfaces ErrInvalidStrategyOverride so
//                       ResolveEffectiveClient routes the request to
//                       legacy with PathReasonInvalidStrategyOverride
//                       and BAML emits its canonical ensure_strategy
//                       error.
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
		return nilIfEmpty(parseBracketedString(vv))
	case []string:
		out := make([]string, 0, len(vv))
		for _, item := range vv {
			if s := normalizeToken(item); s != "" {
				out = append(out, s)
			}
		}
		return nilIfEmpty(out)
	case []any:
		out := make([]string, 0, len(vv))
		for _, item := range vv {
			str, ok := item.(string)
			if !ok {
				// A heterogeneous list with a non-string element is
				// treated as invalid for a strategy override. Parser
				// returns nil; callers via InspectStrategyOverride
				// classify it as present-but-invalid and route to
				// legacy with PathReasonInvalidStrategyOverride.
				return nil
			}
			if s := normalizeToken(str); s != "" {
				out = append(out, s)
			}
		}
		return nilIfEmpty(out)
	default:
		return nil
	}
}

// nilIfEmpty collapses a zero-length slice down to nil so callers that
// gate on `len(chain) > 0` treat "parser accepted the shape but found
// no usable tokens" and "parser rejected the shape outright"
// identically. BAML upstream rejects empty strategy arrays
// (ensure_strategy fails with "strategy must not be empty"); the
// parser mirrors that by returning nil. Whether the absent/valid/
// present-but-invalid classification then routes to the introspected
// chain or to legacy is the caller's decision via
// InspectStrategyOverride — see ParseStrategyOption's doc comment.
func nilIfEmpty(out []string) []string {
	if len(out) == 0 {
		return nil
	}
	return out
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
// ClientA", or half-bracketed "strategy [ClientA" all return nil
// rather than silently collapsing the strategy to a one-element
// override. The caller's three-state classification (via
// InspectStrategyOverride) decides whether the parser-level nil maps
// to "no override" or "present-but-invalid → legacy with
// PathReasonInvalidStrategyOverride". The previous implementation
// accepted all of those shapes; that behaviour diverged from BAML
// upstream and silently changed client resolution on malformed
// runtime registry input.
func parseBracketedString(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "strategy ")
	s = strings.TrimSpace(s)
	// Require an explicit bracketed list after trim + optional
	// `strategy ` prefix stripping. "[]" parses here but yields no
	// tokens, so ParseStrategyOption's nilIfEmpty wrapper collapses
	// it to nil — matching BAML upstream's ensure_strategy rejection
	// of empty arrays. Anything that isn't bracket-delimited is
	// rejected outright.
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil
	}
	// Reject malformed forms that the first/last byte check would
	// otherwise accept: a closing bracket anywhere before the final
	// byte means the input has
	// trailing junk after a complete bracketed list (e.g.
	// "[A] junk ]" or "[A] [B]"), and an extra opening bracket after
	// the first one means the input is doubly-bracketed (e.g.
	// "[[A]]"). Both are silently parsed as bogus tokens by the
	// permissive scan below — better to reject them outright. The
	// parser-level nil result is then classified as present-but-
	// invalid by InspectStrategyOverride and routed to legacy.
	if strings.IndexByte(s[1:len(s)-1], ']') >= 0 {
		return nil
	}
	if strings.IndexByte(s[1:], '[') >= 0 {
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
