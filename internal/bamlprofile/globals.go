package bamlprofile

import (
	stdjson "encoding/json"
	"fmt"

	minijinja "github.com/invakid404/minijinja-go/v2"
	"github.com/invakid404/minijinja-go/v2/value"
)

// BAML v0.223 jinja-runtime magic delimiters (engine/baml-lib/jinja-runtime/
// src/lib.rs:203-204,350-378). The role helper wraps its marker with roleDelim
// on both sides. After rendering, the prompt lowerer (internal/nativeprompt/
// lower.go, a later slice) splits the output on these to reconstruct the
// structured message list. They are internal to the renderer and never reach a
// provider, so reproducing BAML's exact literals is a faithfulness choice that
// keeps this `_` wire-compatible with the existing lower.go when a later slice
// consumes the profile. These mirror internal/nativeprompt/template.go.
const (
	roleDelim = "BAML_CHAT_ROLE_MAGIC_STRING_DELIMITER"

	roleMarkerPrefix = ":baml-start-baml:"
	roleMarkerSuffix = ":baml-end-baml:"

	// allowDupeRoleKey is BAML's reserved role kwarg; it never becomes message
	// metadata and defaults to false.
	allowDupeRoleKey = "__baml_allow_dupe_role__"
	roleKey          = "role"
)

// roleHelper implements the `_` global. `_.role(...)` / `_.chat(...)` emit a
// role marker; the post-render split (nativeprompt/lower.go, a later slice)
// turns each into a new message. Ported verbatim in behavior from
// internal/nativeprompt/env.go, adapted to the fork object protocol (kwargs is
// *value.OrderedMap here, and CallMethod takes value.State).
type roleHelper struct{}

// GetAttr reports that `_` has no attributes; it is method-only (`_.role` /
// `_.chat` are calls, dispatched by CallMethod).
func (roleHelper) GetAttr(string) value.Value { return value.Undefined() }

// CallMethod implements `_.role(...)` / `_.chat(...)`: it validates the role
// (BAML lib.rs:329-348) and returns the role marker the prompt lowerer later
// splits on. Any other method name declines with ErrUnknownMethod.
func (roleHelper) CallMethod(_ value.State, name string, args []value.Value, kwargs *value.OrderedMap) (value.Value, error) {
	if name != "role" && name != "chat" {
		return value.Undefined(), value.ErrUnknownMethod
	}

	// Role from a positional arg or the role= kwarg — error if both or neither
	// (BAML lib.rs:329-348).
	var role string
	haveRolePositional := false
	if len(args) > 1 {
		return value.Undefined(), minijinja.NewError(minijinja.ErrTooManyArguments,
			fmt.Sprintf("bamlprofile: _.role takes at most one positional argument, got %d", len(args)))
	}
	if len(args) == 1 {
		s, ok := args[0].AsString()
		if !ok {
			return value.Undefined(), minijinja.NewError(minijinja.ErrInvalidOperation,
				"bamlprofile: _.role positional argument must be a string")
		}
		role = s
		haveRolePositional = true
	}

	allowDupe := false
	meta := map[string]any{}
	haveRoleKwarg := false
	for _, k := range kwargs.Keys() {
		v, _ := kwargs.Get(k)
		switch k {
		case roleKey:
			s, ok := v.AsString()
			if !ok {
				return value.Undefined(), minijinja.NewError(minijinja.ErrInvalidOperation,
					"bamlprofile: _.role role= must be a string")
			}
			role = s
			haveRoleKwarg = true
		case allowDupeRoleKey:
			b, ok := v.AsBool()
			if !ok {
				return value.Undefined(), minijinja.NewError(minijinja.ErrInvalidOperation,
					fmt.Sprintf("bamlprofile: _.role %s must be a bool", allowDupeRoleKey))
			}
			allowDupe = b
		default:
			meta[k] = toNative(v)
		}
	}

	switch {
	case haveRolePositional && haveRoleKwarg:
		return value.Undefined(), minijinja.NewError(minijinja.ErrTooManyArguments,
			"bamlprofile: _.role got role both positionally and as role= kwarg")
	case !haveRolePositional && !haveRoleKwarg:
		return value.Undefined(), minijinja.NewError(minijinja.ErrMissingArgument,
			"bamlprofile: _.role requires a role (positional or role=)")
	}

	marker, err := emitRoleMarker(role, allowDupe, meta)
	if err != nil {
		// A metadata kwarg that is not JSON-serializable (e.g. a NaN/Inf float)
		// must fail LOUD: emitting a role-less/degraded marker would silently
		// corrupt the message the downstream lowerer reconstructs.
		return value.Undefined(), minijinja.NewError(minijinja.ErrInvalidOperation,
			fmt.Sprintf("bamlprofile: _.role metadata is not JSON-serializable: %v", err))
	}
	return value.FromString(marker), nil
}

// emitRoleMarker builds the role marker string BAML's role helper emits:
// {roleDelim}:baml-start-baml:{json}:baml-end-baml:{roleDelim}. The JSON carries
// role, __baml_allow_dupe_role__, and any additional metadata kwargs
// (BAML lib.rs:350-378). It errors if the payload is not JSON-serializable.
func emitRoleMarker(role string, allowDupe bool, meta map[string]any) (string, error) {
	payload := map[string]any{
		roleKey:          role,
		allowDupeRoleKey: allowDupe,
	}
	for k, v := range meta {
		payload[k] = v
	}
	j, err := marshalJSON(payload)
	if err != nil {
		return "", err
	}
	return roleDelim + roleMarkerPrefix + j + roleMarkerSuffix + roleDelim, nil
}

// ctxObject implements the `ctx` global. Only bare attribute access to
// output_format is modeled in this slice; ctx's complete BAML v0.223 surface is
// measured and expanded in a later slice.
type ctxObject struct {
	outputFormat string
}

// GetAttr exposes ctx.output_format (the only ctx attribute modeled in this
// slice); every other attribute is undefined.
func (c ctxObject) GetAttr(name string) value.Value {
	if name == "output_format" {
		return value.FromString(c.outputFormat)
	}
	return value.Undefined()
}

// toNative converts a minijinja Value used as a role kwarg into a plain Go value
// for the marker JSON. It handles the scalar/map/list shapes a prompt can
// produce (cache_control is a nested {"type": "..."} map). Ported from
// internal/nativeprompt/env.go.
func toNative(v value.Value) any {
	if v.IsNone() || v.IsUndefined() {
		return nil
	}
	if b, ok := v.AsBool(); ok {
		return b
	}
	if i, ok := v.AsInt(); ok {
		return i
	}
	if f, ok := v.AsFloat(); ok {
		return f
	}
	if s, ok := v.AsString(); ok {
		return s
	}
	if sl, ok := v.AsSlice(); ok {
		out := make([]any, 0, len(sl))
		for _, e := range sl {
			out = append(out, toNative(e))
		}
		return out
	}
	if m, ok := v.AsMap(); ok {
		out := make(map[string]any, len(m))
		for k, vv := range m {
			out[k] = toNative(vv)
		}
		return out
	}
	return v.String()
}

// marshalJSON emits deterministic JSON for a marker payload. encoding/json
// already sorts map[string]any keys alphabetically at every level, so no
// pre-sort is needed; the order is not load-bearing anyway, since the marker is
// parsed back into a map structurally (never raw-byte-compared) by the prompt
// lowerer. It returns the marshal error (e.g. a NaN/Inf float) rather than
// emitting a degraded payload, so the caller can fail loud.
func marshalJSON(m map[string]any) (string, error) {
	b, err := stdjson.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
