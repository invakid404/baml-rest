package nativeprompt

import (
	stdjson "encoding/json"
	"fmt"
	"sort"

	mj "github.com/mitsuhiko/minijinja/minijinja-go/v2"
	"github.com/mitsuhiko/minijinja/minijinja-go/v2/syntax"
	"github.com/mitsuhiko/minijinja/minijinja-go/v2/value"
)

// buildEnv constructs the minijinja-Go environment reproducing BAML v0.223's
// get_env() plus the dynamic-template helper layer. outputFormat is the
// pre-rendered ctx.output_format block (BAML installs it as a global before
// render); it may be empty when the template never reaches ctx.output_format.
func buildEnv(outputFormat string) *mj.Environment {
	env := mj.NewEnvironment()

	// trim_blocks + lstrip_blocks, both enabled by BAML (jinja_helpers.rs).
	env.SetWhitespace(syntax.WhitespaceConfig{TrimBlocks: true, LstripBlocks: true})

	// BAML renders prompts as plain text with no HTML auto-escaping.
	env.SetAutoEscapeFunc(func(string) mj.AutoEscape { return mj.AutoEscapeNone })

	// BAML's custom formatter: top-level none renders as "null" (not the empty
	// string minijinja would otherwise produce); everything else renders
	// normally. minijinja-Go's Value.String() does NOT consult ObjectWithString,
	// so object rendering (media markers) is dispatched here explicitly.
	env.SetFormatter(func(_ *mj.State, val value.Value, escape func(string) string) string {
		if val.IsNone() {
			return "null"
		}
		if obj, ok := val.AsObject(); ok {
			if s, ok := obj.(value.ObjectWithString); ok {
				return escape(s.ObjectString())
			}
		}
		return escape(val.String())
	})

	// `_` and its alias `_.chat`/`_.role` role helper.
	env.AddGlobal("_", value.FromObject(roleHelper{}))
	// `ctx` global exposing bare output_format.
	env.AddGlobal("ctx", value.FromObject(ctxObject{outputFormat: outputFormat}))

	return env
}

// roleHelper implements the `_` global. `_.role(...)` / `_.chat(...)` emit a
// role marker; the post-render split (lower.go) turns each into a new message.
type roleHelper struct{}

func (roleHelper) GetAttr(string) value.Value { return value.Undefined() }

func (roleHelper) CallMethod(_ value.State, name string, args []value.Value, kwargs map[string]value.Value) (value.Value, error) {
	if name != "role" && name != "chat" {
		return value.Value{}, value.ErrUnknownMethod
	}

	// Role from a positional arg or the role= kwarg — error if both or neither
	// (BAML lib.rs:329-348).
	var role string
	haveRolePositional := false
	if len(args) > 1 {
		return value.Value{}, fmt.Errorf("nativeprompt: _.role takes at most one positional argument, got %d", len(args))
	}
	if len(args) == 1 {
		s, ok := args[0].AsString()
		if !ok {
			return value.Value{}, fmt.Errorf("nativeprompt: _.role positional argument must be a string")
		}
		role = s
		haveRolePositional = true
	}

	allowDupe := false
	meta := map[string]any{}
	haveRoleKwarg := false
	for k, v := range kwargs {
		switch k {
		case roleKey:
			s, ok := v.AsString()
			if !ok {
				return value.Value{}, fmt.Errorf("nativeprompt: _.role role= must be a string")
			}
			role = s
			haveRoleKwarg = true
		case allowDupeRoleKey:
			b, ok := v.AsBool()
			if !ok {
				// Fail closed on a non-bool allow_dupe, matching the sibling
				// role= validation above, rather than silently coercing to
				// false. This is a test-only spike renderer, not the serving
				// path, so strict rejection here cannot out-strict production.
				return value.Value{}, fmt.Errorf("nativeprompt: _.role %s must be a bool", allowDupeRoleKey)
			}
			allowDupe = b
		default:
			meta[k] = toNative(v)
		}
	}

	switch {
	case haveRolePositional && haveRoleKwarg:
		return value.Value{}, fmt.Errorf("nativeprompt: _.role got role both positionally and as role= kwarg")
	case !haveRolePositional && !haveRoleKwarg:
		return value.Value{}, fmt.Errorf("nativeprompt: _.role requires a role (positional or role=)")
	}

	return value.FromString(emitRoleMarker(role, allowDupe, meta)), nil
}

// emitRoleMarker builds the role marker string BAML's role helper emits:
// {roleDelim}:baml-start-baml:{json}:baml-end-baml:{roleDelim}. The JSON carries
// role, __baml_allow_dupe_role__, and any additional metadata kwargs.
func emitRoleMarker(role string, allowDupe bool, meta map[string]any) string {
	payload := map[string]any{
		roleKey:          role,
		allowDupeRoleKey: allowDupe,
	}
	for k, v := range meta {
		payload[k] = v
	}
	return roleDelim + roleMarkerPrefix + marshalJSON(payload) + roleMarkerSuffix + roleDelim
}

// ctxObject implements the `ctx` global. Only bare attribute access to
// output_format is supported; the callable form ctx.output_format(...) is a
// declined feature (see Supports) and is not reachable from this template.
type ctxObject struct {
	outputFormat string
}

func (c ctxObject) GetAttr(name string) value.Value {
	if name == "output_format" {
		return value.FromString(c.outputFormat)
	}
	return value.Undefined()
}

// mediaObject implements a BamlValue::Media as a minijinja object: truthy (so
// {% if p.img %} passes) and rendered (via the formatter's ObjectWithString
// dispatch) as a media marker that the post-render split reconstructs into a
// MediaPart.
type mediaObject struct {
	kind   string
	url    string
	base64 string
	mime   string
}

func newMediaObject(m *Media) mediaObject {
	return mediaObject{kind: string(m.Kind), url: m.URL, base64: m.Base64, mime: m.Mime}
}

func (mediaObject) GetAttr(string) value.Value { return value.Undefined() }
func (mediaObject) ObjectIsTrue() bool         { return true }

func (m mediaObject) ObjectString() string {
	payload := map[string]any{"kind": m.kind}
	if m.url != "" {
		payload["url"] = m.url
	}
	if m.base64 != "" {
		payload["base64"] = m.base64
	}
	if m.mime != "" {
		payload["mime"] = m.mime
	}
	return mediaDelim + mediaMarkerPrefix + marshalJSON(payload) + mediaMarkerSuffix + mediaDelim
}

// toNative converts a minijinja Value used as a role kwarg into a plain Go
// value for the marker JSON. It handles the scalar/map/list shapes the dynamic
// template can produce (cache_control is a nested {"type": "..."} map).
func toNative(v value.Value) any {
	switch {
	case v.IsNone() || v.IsUndefined():
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

// marshalJSON emits deterministic JSON (sorted keys) for a marker payload.
// Determinism is not strictly required — markers are parsed back into maps and
// compared structurally — but it keeps golden output stable.
func marshalJSON(m map[string]any) string {
	b, err := stdjson.Marshal(sortedMap(m))
	if err != nil {
		// Marker payloads are simple JSON-safe maps; a failure is a programming
		// error, not runtime data. Surface it inline rather than panicking in a
		// template render.
		return fmt.Sprintf("{\"__marshal_error__\":%q}", err.Error())
	}
	return string(b)
}

// sortedMap wraps a map so encoding/json emits keys in sorted order. (Go's
// encoding/json already sorts string map keys; sortedMap makes the intent
// explicit and guards nested maps.)
func sortedMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if nested, ok := m[k].(map[string]any); ok {
			out[k] = sortedMap(nested)
		} else {
			out[k] = m[k]
		}
	}
	return out
}
