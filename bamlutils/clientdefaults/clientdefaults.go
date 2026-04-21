// Package clientdefaults provides deployment-wide defaults for BAML
// ClientRegistry option values. Defaults are parsed once at worker startup
// and merged into every caller's client Options map before the registry is
// handed to the BAML SDK.
//
// Configuration is loaded from the BAML_REST_CLIENT_DEFAULTS environment
// variable. The value is a JSON object:
//
//	{
//	  "client_defaults": {
//	    "options": {
//	      "allowed_role_metadata": ["cache_control"]
//	    }
//	  }
//	}
//
// v1 recognizes exactly one option, allowed_role_metadata. The envelope is
// designed to accept additional options additively in future versions.
//
// # Merge contract
//
// For each handler and each client in the registry:
//
//   - If the caller did not set the handler's key in client.Options, the
//     deployment default is injected (a freshly-allocated clone, so
//     concurrent requests don't alias shared state).
//   - If the caller set the key with any value, including null, [], "",
//     or "none", the caller wins and the default is not applied.
//
// # Advertised opt-outs for allowed_role_metadata
//
// To opt out of the deployment default for a particular request, callers
// should set one of:
//
//   - "allowed_role_metadata": []     — BAML parses as empty Only([])
//   - "allowed_role_metadata": "none" — BAML parses as AllowedRoleMetadata::None
//
// Both produce "no metadata allowed" end-to-end and are BAML-valid.
//
// # Anti-pattern: null
//
// "allowed_role_metadata": null is respected by this package's merge rule
// (caller wins, default not applied), but BAML's ensure_allowed_metadata
// rejects non-array/non-string values at LLM-client construction time, so
// the request fails. Do not advertise null as an opt-out.
//
// # BuildRequest caveat
//
// When BAML_REST_USE_BUILD_REQUEST=true, message-level metadata (including
// cache_control) is dropped by BAML's BuildRequest serializer regardless of
// the allowed_role_metadata default configured here. The worker emits a
// startup warning when both conditions hold.
package clientdefaults

import (
	"bytes"
	"fmt"
	"os"
	"sort"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/bamlutils"
)

// EnvVar is the environment variable name read by Load.
const EnvVar = "BAML_REST_CLIENT_DEFAULTS"

// Config is the parsed, validated deployment config. Immutable after Load.
type Config struct {
	// parsed holds one entry per handler whose key appeared in the config.
	// The value is the output of Handler.Parse. Entries preserve the
	// registry's handler order for deterministic Apply iteration.
	parsed []parsedEntry
}

type parsedEntry struct {
	handler *Handler
	value   any
}

// envelope is the top-level shape of BAML_REST_CLIENT_DEFAULTS. Fields are
// decoded with DisallowUnknownFields so typos in envelope-level keys are
// rejected loudly.
type envelope struct {
	ClientDefaults *clientDefaultsBody `json:"client_defaults"`
}

type clientDefaultsBody struct {
	Options map[string]json.RawMessage `json:"options"`
}

// Load reads BAML_REST_CLIENT_DEFAULTS and returns a non-nil *Config. An
// empty or unset env var returns a Config whose Apply is a no-op.
//
// Errors are returned for:
//   - malformed JSON
//   - unknown envelope keys (caught by DisallowUnknownFields)
//   - unknown option keys (no handler registered for the key)
//   - handler-specific Parse errors (wrong value shape)
func Load() (*Config, error) {
	raw := os.Getenv(EnvVar)
	return parse(raw)
}

func parse(raw string) (*Config, error) {
	cfg := &Config{}
	if raw == "" {
		return cfg, nil
	}

	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var env envelope
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvVar, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s: unexpected trailing data", EnvVar)
	}

	if env.ClientDefaults == nil {
		return cfg, nil
	}

	// Validate option keys against the handler registry. Preserve handler
	// order for deterministic Apply iteration; sort unknown-key errors for
	// stable error messages regardless of Go map iteration order.
	unknown := make([]string, 0)
	for key := range env.ClientDefaults.Options {
		if handlerByKey(key) == nil {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf(
			"%s: unknown option key(s): %v (known keys: %v)",
			EnvVar, unknown, knownKeys())
	}

	for i := range registry {
		h := &registry[i]
		raw, ok := env.ClientDefaults.Options[h.Key]
		if !ok {
			continue
		}
		value, err := h.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvVar, err)
		}
		cfg.parsed = append(cfg.parsed, parsedEntry{handler: h, value: value})
	}

	return cfg, nil
}

func knownKeys() []string {
	out := make([]string, 0, len(registry))
	for i := range registry {
		out = append(out, registry[i].Key)
	}
	sort.Strings(out)
	return out
}

// HasKey reports whether the given option key was configured. Used by the
// worker to emit narrow warnings (e.g. the BuildRequest caveat).
func (c *Config) HasKey(key string) bool {
	if c == nil {
		return false
	}
	for _, e := range c.parsed {
		if e.handler.Key == key {
			return true
		}
	}
	return false
}

// Apply merges deployment defaults into each client's Options map in the
// registry. For each handler, calls the handler's Apply function with the
// caller's existing value (if any) to decide what to set.
//
// Apply is safe to call concurrently from multiple goroutines on the same
// Config: handlers produce freshly-allocated values for every client, so no
// state is shared across calls.
func (c *Config) Apply(registry *bamlutils.ClientRegistry) {
	if c == nil || len(c.parsed) == 0 || registry == nil {
		return
	}
	for _, client := range registry.Clients {
		if client == nil {
			continue
		}
		for _, entry := range c.parsed {
			existing, present := client.Options[entry.handler.Key]
			result, set := entry.handler.Apply(entry.value, existing, present)
			if !set {
				continue
			}
			if client.Options == nil {
				client.Options = map[string]any{}
			}
			client.Options[entry.handler.Key] = result
		}
	}
}
