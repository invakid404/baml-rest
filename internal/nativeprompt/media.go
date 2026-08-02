package nativeprompt

import (
	stdjson "encoding/json"
	"fmt"

	"github.com/invakid404/minijinja-go/v2/value"
)

// mediaObject implements a BamlValue::Media as a fork MiniJinja object: truthy
// (so {% if p.img %} passes) and rendered — via the fork's Value.String, which
// dispatches value.ObjectWithString itself (v2.16.0-baml.4, PATCHES #104) — as a
// media marker that the post-render split reconstructs into a MediaPart.
//
// It is deliberately owned HERE and not by internal/bamlprofile. The profile has
// no media host value at all: BAML's serde media marker has a different shape
// from this renderer's marker protocol, and lowering it into a provider media
// body is prompt-integration work the generic leaf has no path for (#602). So
// bamlprofile.ClassValue / ListValue REJECT this object rather than render it
// unlike BAML, and this marker never enters a profile host value — it is bound
// only into the fork-native message tree built in input.go.
//
// Live admission declines media entirely; [Supports] additionally declines every
// kind but image. This object therefore only ever renders for the renderer's own
// image corpus, which the stock-BAML differential covers.
type mediaObject struct {
	kind   string
	url    string
	base64 string
	mime   string
}

func newMediaObject(m *Media) mediaObject {
	return mediaObject{kind: string(m.Kind), url: m.URL, base64: m.Base64, mime: m.Mime}
}

// GetAttr reports no attributes: a media value is rendered and tested for truth,
// never traversed.
func (mediaObject) GetAttr(string) value.Value { return value.Undefined() }

// ObjectIsTrue makes a present media part truthy, so the template's
// {% elif p.img %} branch selects it.
func (mediaObject) ObjectIsTrue() bool { return true }

// ObjectString renders the media marker the lowerer splits back into a
// MediaPart: {mediaDelim}:baml-start-media:{json}:baml-end-media:{mediaDelim}.
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
	return mediaDelim + mediaMarkerPrefix + marshalMarkerJSON(payload) + mediaMarkerSuffix + mediaDelim
}

// marshalMarkerJSON emits the marker payload as JSON. encoding/json already
// sorts map[string]any keys at every level, so the output is deterministic; the
// order is not load-bearing either way, since [parseMediaMarker] unmarshals it
// structurally rather than comparing bytes.
//
// The payload is built entirely from this package's own string fields, so a
// marshal failure is impossible from data; surfacing it inline (rather than
// panicking mid-render) keeps a programming error visible in the rendered output
// instead of taking down a request.
func marshalMarkerJSON(m map[string]any) string {
	b, err := stdjson.Marshal(m)
	if err != nil {
		return fmt.Sprintf("{\"__marshal_error__\":%q}", err.Error())
	}
	return string(b)
}
