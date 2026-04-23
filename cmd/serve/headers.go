package main

import (
	"net/http"
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/bamlutils"
)

// Header names for per-request routing/retry metadata. Values of the
// X-BAML-Client and X-BAML-Winner-Client headers echo the client name as
// configured; dropping rather than sanitising a bad value is the safest
// default (see setBAMLHeaders).
const (
	HeaderBAMLPath             = "X-BAML-Path"
	HeaderBAMLPathReason       = "X-BAML-Path-Reason"
	HeaderBAMLClient           = "X-BAML-Client"
	HeaderBAMLWinnerClient     = "X-BAML-Winner-Client"
	HeaderBAMLWinnerProvider   = "X-BAML-Winner-Provider"
	HeaderBAMLRetryMax         = "X-BAML-Retry-Max"
	HeaderBAMLRetryCount       = "X-BAML-Retry-Count"
	HeaderBAMLUpstreamDuration = "X-BAML-Upstream-Duration-Ms"
	HeaderBAMLBamlCallCount    = "X-BAML-Baml-Call-Count"
)

// headerSetter abstracts over net/http.Header.Set and fiber.Ctx.Set so the
// same emission logic drives both unary servers.
type headerSetter func(name, value string)

func netHTTPHeaderSetter(h http.Header) headerSetter {
	return func(name, value string) { h.Set(name, value) }
}

func fiberHeaderSetter(c fiber.Ctx) headerSetter {
	return func(name, value string) { c.Set(name, value) }
}

// setBAMLHeaders emits the compact routing/retry header set for a unary
// response, given the JSON-encoded planned and outcome metadata payloads
// (either may be nil; fields are omitted when their source is absent).
//
// The set is intentionally narrow — chain details and retry-policy encoding
// live only in the streaming JSON payload. Headers are for debuggability
// (curl -i), not full observability. See the plan's §5 for the shortlist.
func setBAMLHeaders(setter headerSetter, planned, outcome *bamlutils.Metadata) {
	if planned != nil {
		if planned.Path != "" {
			setter(HeaderBAMLPath, planned.Path)
		}
		if planned.PathReason != "" {
			setter(HeaderBAMLPathReason, planned.PathReason)
		}
		if v := sanitizeTokenHeader(planned.Client); v != "" {
			setter(HeaderBAMLClient, v)
		}
		if planned.RetryMax != nil {
			setter(HeaderBAMLRetryMax, strconv.Itoa(*planned.RetryMax))
		}
	}
	if outcome != nil {
		// WinnerClient is only emitted when it differs from the planned
		// Client to avoid noise on single-provider routes where the two
		// are tautologically identical.
		if outcome.WinnerClient != "" && (planned == nil || outcome.WinnerClient != planned.Client) {
			if v := sanitizeTokenHeader(outcome.WinnerClient); v != "" {
				setter(HeaderBAMLWinnerClient, v)
			}
		}
		if outcome.WinnerProvider != "" {
			if v := sanitizeTokenHeader(outcome.WinnerProvider); v != "" {
				setter(HeaderBAMLWinnerProvider, v)
			}
		}
		if outcome.RetryCount != nil {
			setter(HeaderBAMLRetryCount, strconv.Itoa(*outcome.RetryCount))
		}
		if outcome.UpstreamDurMs != nil {
			setter(HeaderBAMLUpstreamDuration, strconv.FormatInt(*outcome.UpstreamDurMs, 10))
		}
		if outcome.BamlCallCount != nil {
			setter(HeaderBAMLBamlCallCount, strconv.Itoa(*outcome.BamlCallCount))
		}
	}
}

// decodeMetadataJSON parses a JSON-encoded bamlutils.Metadata payload.
// Returns nil on parse failure rather than propagating the error — a bad
// payload should never fail the whole response, and absence is
// semantically equivalent to "unknown" on the header side anyway.
func decodeMetadataJSON(data []byte) *bamlutils.Metadata {
	if len(data) == 0 {
		return nil
	}
	var md bamlutils.Metadata
	if err := json.Unmarshal(data, &md); err != nil {
		return nil
	}
	return &md
}

// sanitizeTokenHeader drops any value containing characters that would
// produce an invalid HTTP header value. BAML client/provider names are
// normally safe (letters, digits, dashes, dots, underscores), but we drop
// rather than coerce — silently truncating names would be worse than an
// absent header.
func sanitizeTokenHeader(v string) string {
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < 0x20 || c == 0x7f || c == '\r' || c == '\n' {
			return ""
		}
	}
	return v
}
