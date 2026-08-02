package nativeprompt

import (
	"github.com/invakid404/minijinja-go/v2/value"
)

// MediaKind is the kind of a media content part. Only image is proven parity in
// this spike (see Supports); the others exist so the input model is complete
// and so the decline predicate has something concrete to reject.
type MediaKind string

const (
	MediaImage MediaKind = "image"
	MediaAudio MediaKind = "audio"
	MediaPDF   MediaKind = "pdf"
	MediaVideo MediaKind = "video"
)

// Media is a media value inside a content part. Exactly one of URL / Base64 is
// set; Mime is optional and travels with base64 media.
type Media struct {
	Kind   MediaKind
	URL    string
	Base64 string
	Mime   string
}

// CacheControl is Anthropic prompt-caching metadata attached to a message.
type CacheControl struct {
	// Type is the cache_control type, e.g. "ephemeral". It is surfaced to the
	// template as m.metadata.cache_control.cache_type, matching the generated
	// dynamic input (dynamic.baml aliases the reserved word `type`).
	Type string
}

// Metadata is per-message metadata.
type Metadata struct {
	CacheControl *CacheControl
}

// ContentPart mirrors the generated Baml_Rest_ContentPart: at most one of the
// content fields is set. OutputFormat marks the bare ctx.output_format part.
type ContentPart struct {
	Text         *string
	Image        *Media
	Audio        *Media
	PDF          *Media
	Video        *Media
	OutputFormat bool
}

// Message mirrors the generated Baml_Rest_Message: a role, either legacy
// Content or multi-part Parts, and optional Metadata. When Parts is non-empty
// it takes precedence over Content, matching the template's {% if m.parts %}
// ... {% elif m.content %} branch.
type Message struct {
	Role     string
	Content  *string
	Parts    []ContentPart
	Metadata *Metadata
}

// messagesToValue lowers the input messages into the minijinja value the
// template iterates over. The shape mirrors the generated internal message
// (role / content / parts / metadata.cache_control.cache_type) so the template
// expressions resolve exactly as they do under BAML.
//
// The tree is built from fork-NATIVE containers (value.FromMap / value.FromSlice)
// rather than bamlprofile.ClassValue / ListValue. That is forced, not incidental:
// a content part may hold [mediaObject], and the profile's host containers reject
// any object that is not one of their own so they can never render unlike BAML
// (see rendercontext.go and #602). The admitted template only ever does attribute
// access, truth tests and iteration over this tree — never a direct {{ m }} /
// {{ m.parts }} render, the one place a host class/list differs from a native
// container — and the stock-BAML differential covers every admitted row.
func messagesToValue(msgs []Message) value.Value {
	out := make([]value.Value, 0, len(msgs))
	for i := range msgs {
		out = append(out, messageToValue(&msgs[i]))
	}
	return value.FromSlice(out)
}

func messageToValue(m *Message) value.Value {
	fields := map[string]value.Value{
		"role": value.FromString(m.Role),
	}

	// metadata is only present when cache_control is present, so that
	// `m.metadata` is undefined (falsy) otherwise — matching the generated
	// input where an absent metadata object is omitted.
	if m.Metadata != nil && m.Metadata.CacheControl != nil {
		fields["metadata"] = value.FromMap(map[string]value.Value{
			"cache_control": value.FromMap(map[string]value.Value{
				"cache_type": value.FromString(m.Metadata.CacheControl.Type),
			}),
		})
	}

	if len(m.Parts) > 0 {
		parts := make([]value.Value, 0, len(m.Parts))
		for i := range m.Parts {
			parts = append(parts, partToValue(&m.Parts[i]))
		}
		fields["parts"] = value.FromSlice(parts)
	} else if m.Content != nil {
		fields["content"] = value.FromString(*m.Content)
	}

	return value.FromMap(fields)
}

func partToValue(p *ContentPart) value.Value {
	fields := map[string]value.Value{}
	switch {
	case p.Text != nil:
		fields["text"] = value.FromString(*p.Text)
	case p.Image != nil:
		fields["img"] = value.FromObject(newMediaObject(p.Image))
	case p.Audio != nil:
		fields["aud"] = value.FromObject(newMediaObject(p.Audio))
	case p.PDF != nil:
		fields["doc"] = value.FromObject(newMediaObject(p.PDF))
	case p.Video != nil:
		fields["vid"] = value.FromObject(newMediaObject(p.Video))
	case p.OutputFormat:
		fields["output_format"] = value.FromBool(true)
	}
	return value.FromMap(fields)
}
