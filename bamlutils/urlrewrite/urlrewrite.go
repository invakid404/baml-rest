// Package urlrewrite provides URL rewrite rules for remapping LLM provider
// base URLs. This is used both at build time (search-and-replace in .baml
// files) and at runtime (rewriting base_url in custom client options and
// outgoing HTTP requests).
//
// Rules are configured via the BAML_REST_BASE_URL_REWRITES environment
// variable or the --base-url-rewrite flag. Format:
//
//	BAML_REST_BASE_URL_REWRITES="https://llm.mandel.ai=http://litellm:4000;https://other.ai=http://local:8000"
package urlrewrite

import (
	"net/url"
	"os"
	"strings"
	"sync"
)

// Rule represents a URL rewrite rule: occurrences of From are replaced with To.
type Rule struct {
	From string
	To   string
}

// ParseRules parses URL rewrite rules from a semicolon-separated string.
// Each rule is in the format "from=to". Whitespace around separators is trimmed.
func ParseRules(s string) []Rule {
	if s == "" {
		return nil
	}
	var rules []Rule
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Split on first '=' only (URLs may contain '=' in query params,
		// but base URLs typically don't)
		idx := strings.Index(part, "=")
		if idx <= 0 {
			continue
		}
		from := strings.TrimSpace(part[:idx])
		to := strings.TrimSpace(part[idx+1:])
		if from != "" && to != "" {
			rules = append(rules, Rule{From: from, To: to})
		}
	}
	return rules
}

// Apply performs a dumb search-and-replace of all rules on the input string.
// Used for build-time .baml file rewriting.
func Apply(s string, rules []Rule) string {
	for _, r := range rules {
		s = strings.ReplaceAll(s, r.From, r.To)
	}
	return s
}

func applyURLPrefixRule(rawURL string, rule Rule) string {
	urlParsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	fromParsed, err := url.Parse(rule.From)
	if err != nil {
		return ""
	}
	toParsed, err := url.Parse(rule.To)
	if err != nil {
		return ""
	}

	if urlParsed.Scheme != fromParsed.Scheme || urlParsed.Host != fromParsed.Host {
		return ""
	}

	basePath := fromParsed.EscapedPath()
	if basePath == "" {
		basePath = "/"
	}
	urlPath := urlParsed.EscapedPath()
	if urlPath == "" {
		urlPath = "/"
	}
	if urlPath != basePath && !strings.HasPrefix(urlPath, strings.TrimRight(basePath, "/")+"/") {
		return ""
	}

	newURL := *urlParsed
	newURL.Scheme = toParsed.Scheme
	newURL.Host = toParsed.Host

	matchedPathPrefix := strings.TrimRight(fromParsed.Path, "/")
	matchedRawPathPrefix := strings.TrimRight(basePath, "/")
	toEscapedPath := toParsed.EscapedPath()
	joinedPathPrefix := strings.TrimRight(toParsed.Path, "/")
	joinedRawPathPrefix := strings.TrimRight(toEscapedPath, "/")
	suffixPath := strings.TrimPrefix(urlParsed.Path, matchedPathPrefix)
	suffixRawPath := strings.TrimPrefix(urlPath, matchedRawPathPrefix)

	newURL.Path = joinedPathPrefix + suffixPath
	newURL.RawPath = joinedRawPathPrefix + suffixRawPath
	if newURL.RawPath == newURL.Path {
		newURL.RawPath = ""
	}

	return newURL.String()
}

// ApplyToURL rewrites a URL by matching scheme/host/path boundaries against a
// rule's From URL. Only the first matching rule is applied. Used for runtime
// URL rewriting of base_url values and outgoing HTTP request URLs.
func ApplyToURL(url string, rules []Rule) string {
	for _, r := range rules {
		if rewritten := applyURLPrefixRule(url, r); rewritten != "" {
			return rewritten
		}
	}
	return url
}

// builtinRules is set at compile time via -ldflags:
//
//	-X github.com/invakid404/baml-rest/bamlutils/urlrewrite.builtinRules=from1=to1;from2=to2
//
// These are the default rewrite rules baked into the binary at build time.
// They can be overridden at runtime by setting BAML_REST_BASE_URL_REWRITES.
var builtinRules string

var (
	globalRulesOnce sync.Once
	globalRules     []Rule
)

// GlobalRules returns the active URL rewrite rules. Resolution order:
//  1. BAML_REST_BASE_URL_REWRITES env var (if set, fully overrides builtin rules)
//  2. Builtin rules baked in at compile time via -ldflags
//
// Parsed once and cached for the process lifetime.
func GlobalRules() []Rule {
	globalRulesOnce.Do(func() {
		if envVal := os.Getenv("BAML_REST_BASE_URL_REWRITES"); envVal != "" {
			globalRules = ParseRules(envVal)
		} else {
			globalRules = ParseRules(builtinRules)
		}
	})
	return globalRules
}

// ResetGlobalRules clears the cached global rules. Only for testing.
func ResetGlobalRules() {
	globalRulesOnce = sync.Once{}
	globalRules = nil
}
