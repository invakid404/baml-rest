package bamlutils

import (
	"errors"
	"fmt"
)

// MediaKind identifies a BAML media type (image, audio, pdf, video).
type MediaKind int

const (
	MediaKindImage MediaKind = iota
	MediaKindAudio
	MediaKindPDF
	MediaKindVideo
)

// String returns the lowercase name of the media kind.
func (k MediaKind) String() string {
	switch k {
	case MediaKindImage:
		return "image"
	case MediaKindAudio:
		return "audio"
	case MediaKindPDF:
		return "pdf"
	case MediaKindVideo:
		return "video"
	default:
		return fmt.Sprintf("MediaKind(%d)", int(k))
	}
}

// ConstName returns the Go constant name for this media kind (e.g., "MediaKindImage").
// Used by the code generator to emit references to these constants.
func (k MediaKind) ConstName() string {
	switch k {
	case MediaKindImage:
		return "MediaKindImage"
	case MediaKindAudio:
		return "MediaKindAudio"
	case MediaKindPDF:
		return "MediaKindPDF"
	case MediaKindVideo:
		return "MediaKindVideo"
	default:
		return fmt.Sprintf("MediaKind(%d)", int(k))
	}
}

// MediaInput is a JSON-friendly representation of a BAML media value.
// Callers must provide exactly one of URL or Base64.
type MediaInput struct {
	URL       *string `json:"url,omitempty"`
	Base64    *string `json:"base64,omitempty"`
	MediaType *string `json:"media_type,omitempty"`
}

// Validate checks that exactly one of URL or Base64 is set.
func (m *MediaInput) Validate() error {
	hasURL := m.URL != nil
	hasBase64 := m.Base64 != nil

	if hasURL && hasBase64 {
		return errors.New("must provide either 'url' or 'base64', not both")
	}
	if !hasURL && !hasBase64 {
		return errors.New("must provide either 'url' or 'base64'")
	}

	return nil
}

// ConvertMedia validates a MediaInput and converts it to a BAML media object
// via the adapter's constructor methods. The returned value is the opaque BAML
// media interface (e.g., baml.Image) typed as any, since bamlutils cannot
// import the BAML client package.
func ConvertMedia(adapter Adapter, kind MediaKind, input *MediaInput) (any, error) {
	if err := input.Validate(); err != nil {
		return nil, fmt.Errorf("%s input: %w", kind, err)
	}

	if input.URL != nil {
		return adapter.NewMediaFromURL(kind, *input.URL, input.MediaType)
	}

	return adapter.NewMediaFromBase64(kind, *input.Base64, input.MediaType)
}
