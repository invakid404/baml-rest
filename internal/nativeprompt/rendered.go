package nativeprompt

import (
	stdjson "encoding/json"
	"fmt"
)

// Kind distinguishes BAML's two RenderedPrompt shapes.
type Kind string

const (
	// KindCompletion is BAML's RenderedPrompt::Completion: the rendered output
	// contained no role and no media delimiter, so it is a single opaque
	// completion string.
	KindCompletion Kind = "completion"
	// KindChat is BAML's RenderedPrompt::Chat: an ordered list of role-tagged
	// messages, each with ordered content parts.
	KindChat Kind = "chat"
)

// RenderedPrompt is the native equivalent of BAML's RenderedPrompt. It is the
// unit of comparison in the differential harness: two RenderedPrompt values are
// equal iff their canonical JSON encodings are byte-identical.
type RenderedPrompt struct {
	Kind Kind `json:"kind"`
	// Completion is set only when Kind == KindCompletion.
	Completion string `json:"completion,omitempty"`
	// Messages is set only when Kind == KindChat.
	Messages []RenderedMessage `json:"messages,omitempty"`
}

// RenderedMessage is one role-tagged chat message with ordered content parts.
type RenderedMessage struct {
	Role string `json:"role"`
	// AllowDuplicateRole mirrors BAML's per-message allow_duplicate_role, driven
	// by the _.role(..., __baml_allow_dupe_role__=...) kwarg (default false).
	AllowDuplicateRole bool `json:"allow_duplicate_role"`
	// Meta carries the role helper's non-reserved kwargs as message metadata —
	// most importantly cache_control. Nil when the message has no metadata.
	Meta  map[string]any `json:"meta,omitempty"`
	Parts []Part         `json:"parts"`
}

// Part is a single content part: exactly one of Text / Media is set.
type Part struct {
	Text  *string    `json:"text,omitempty"`
	Media *MediaPart `json:"media,omitempty"`
}

// MediaPart is a reconstructed media content part. Kind is the media kind
// (image/audio/pdf/video). Exactly one of URL / Base64 is populated; Mime is
// set for base64 media.
type MediaPart struct {
	Kind   string `json:"kind"`
	URL    string `json:"url,omitempty"`
	Base64 string `json:"base64,omitempty"`
	Mime   string `json:"mime,omitempty"`
}

// textPart builds a text Part.
func textPart(s string) Part { return Part{Text: &s} }

// Canonical returns the deterministic JSON encoding used for byte-exact
// comparison. encoding/json sorts map keys, so Meta ordering is stable.
func (p *RenderedPrompt) Canonical() ([]byte, error) {
	b, err := stdjson.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("nativeprompt: canonical encode: %w", err)
	}
	return b, nil
}

// Equal reports whether two RenderedPrompt values have identical canonical
// encodings, returning the two encodings for diagnostics on mismatch.
func Equal(a, b *RenderedPrompt) (equal bool, aJSON, bJSON []byte, err error) {
	aJSON, err = a.Canonical()
	if err != nil {
		return false, nil, nil, err
	}
	bJSON, err = b.Canonical()
	if err != nil {
		return false, nil, nil, err
	}
	return string(aJSON) == string(bJSON), aJSON, bJSON, nil
}
