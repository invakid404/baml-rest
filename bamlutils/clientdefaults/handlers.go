package clientdefaults

import (
	"encoding/json"
	"fmt"
)

// Handler describes how one ClientRegistry option key is parsed from the
// deployment config and merged into each caller's client Options map.
//
// Adding a new deployment-wide default is a new Handler struct literal
// appended to registry plus a handler test case. No changes to Load, Apply,
// or Config are required.
type Handler struct {
	// Key is the BAML client option this handler manages. It corresponds to
	// a key inside `client_registry.clients[].options`.
	Key string

	// Parse validates and converts the JSON value from the config into
	// whatever opaque Go form Apply expects. Called once per Load.
	Parse func(raw json.RawMessage) (any, error)

	// Apply is called per client per Apply() invocation. Arguments:
	//   parsed   — the value returned by Parse.
	//   existing — the caller's current value for this key in their Options
	//              map, or nil if the key wasn't present.
	//   present  — true if the caller set the key in their Options map
	//              (including explicit null, empty list, etc.).
	// Returns (out, true) to set client.Options[Key] = out, or (nil, false)
	// to leave the caller's value unchanged.
	//
	// Implementers MUST return a freshly-allocated value (no shared
	// references to `parsed`) so multiple clients within the same request
	// and multiple concurrent requests don't alias the same slice/map.
	// Use cloneAsJSONValue for the common case.
	Apply func(parsed, existing any, present bool) (any, bool)
}

// registry is the set of handlers recognized by Load and Apply. The order
// does not matter: each handler manages its own key and operates
// independently of the others.
var registry = []Handler{
	allowedRoleMetadataHandler,
}

// allowedRoleMetadataHandler manages the `allowed_role_metadata` option.
//
// BAML's runtime uses this option to decide which message-level metadata keys
// survive the message→request-body conversion. When the option is absent, BAML
// defaults to AllowedRoleMetadata::None, which silently strips every key —
// including `cache_control` for Anthropic prompt caching. This handler lets
// operators pin a deployment-wide default so callers don't have to remember
// to set it on every client.
//
// Accepts the same value shapes BAML accepts: "all", "none", or a list of
// strings. Any caller-provided value (including null, "") wins over the
// deployment default — see the package doc comment for the merge contract.
var allowedRoleMetadataHandler = Handler{
	Key: "allowed_role_metadata",
	Parse: func(raw json.RawMessage) (any, error) {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("allowed_role_metadata: invalid JSON: %w", err)
		}
		switch typed := v.(type) {
		case string:
			// BAML's parser only accepts the literal strings "all" and "none"
			// for the non-list form (see engine/baml-lib/llm-client/src/
			// clientspec.rs:461). Reject other strings at startup so a typo
			// like "cache_control" fails loudly in the worker instead of
			// silently passing through and exploding at LLM-client
			// construction time on the first request.
			if typed != "all" && typed != "none" {
				return nil, fmt.Errorf(
					"allowed_role_metadata string must be \"all\" or \"none\", got %q", typed)
			}
			return typed, nil
		case []any:
			for i, elem := range typed {
				if _, ok := elem.(string); !ok {
					return nil, fmt.Errorf(
						"allowed_role_metadata[%d] must be a string, got %T", i, elem)
				}
			}
			return typed, nil
		default:
			return nil, fmt.Errorf(
				"allowed_role_metadata must be \"all\", \"none\", or an array of strings, got %T", v)
		}
	},
	Apply: func(parsed, _ any, present bool) (any, bool) {
		if present {
			return nil, false
		}
		return cloneAsJSONValue(parsed), true
	},
}

// cloneAsJSONValue deep-copies a value in the shapes produced by
// json.Unmarshal into map[string]any: []any, map[string]any, and primitive
// scalars. Returns the input unchanged for types that aren't part of that
// shape space.
//
// Handlers use this to satisfy the fresh-allocation contract of Handler.Apply.
func cloneAsJSONValue(v any) any {
	switch typed := v.(type) {
	case []any:
		out := make([]any, len(typed))
		for i, elem := range typed {
			out[i] = cloneAsJSONValue(elem)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, elem := range typed {
			out[k] = cloneAsJSONValue(elem)
		}
		return out
	default:
		return v
	}
}

// handlerByKey returns the registered handler for the given option key, or
// nil if no handler is registered. Used by Load for unknown-key rejection.
func handlerByKey(key string) *Handler {
	for i := range registry {
		if registry[i].Key == key {
			return &registry[i]
		}
	}
	return nil
}
