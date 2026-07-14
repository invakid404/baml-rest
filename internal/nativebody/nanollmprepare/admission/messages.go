package admission

import (
	"unicode/utf8"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
)

// admittedRoles is the proven standard OpenAI chat role set — the same three
// roles the native body builder proves byte-exact against BAML. Any other role
// (tool/function/developer/custom, or a remapped/default role) declines.
var admittedRoles = map[string]struct{}{
	"system":    {},
	"user":      {},
	"assistant": {},
}

// validateMessages is the StageMessage gate on the GENERATED dynamic messages,
// evaluated on the input shape BEFORE render so a decline carries a precise,
// secret-free reason. It requires a non-empty ordered list of standard-role,
// metadata-free messages, each with at least one part and every part text-only
// (output_format is allowed — it renders to text) and valid UTF-8. Media,
// message metadata, custom roles, and unknown/empty parts decline. It never
// references a text VALUE beyond checking UTF-8 validity, so a decline can never
// leak prompt content.
func validateMessages(msgs []bamlutils.DynamicMessage) *Decline {
	if len(msgs) == 0 {
		return declinef(StageMessage, ReasonEmptyMessages, "dynamic prompt has no messages")
	}
	for i := range msgs {
		m := &msgs[i]
		if _, ok := admittedRoles[m.Role]; !ok {
			return declinef(StageMessage, ReasonRoleUnsupported,
				"message %d role is outside the proven system/user/assistant set", i)
		}
		// Reject ANY metadata object, not just a populated cache_control: the
		// proven claim is metadata-free, and a metadata carrier native does not
		// reproduce (present or empty) must fall back to BAML.
		if m.Metadata != nil {
			return declinef(StageMessage, ReasonMessageMetadata,
				"message %d carries a metadata object outside the proven text-only claim", i)
		}
		switch {
		case len(m.PartsContent) > 0:
			for j := range m.PartsContent {
				if d := validatePart(i, j, &m.PartsContent[j]); d != nil {
					return d
				}
			}
		case m.TextContent != nil:
			if !utf8.ValidString(*m.TextContent) {
				return declinef(StageMessage, ReasonInvalidUTF8, "message %d content is not valid UTF-8", i)
			}
		default:
			return declinef(StageMessage, ReasonEmptyMessage, "message %d has no content", i)
		}
	}
	return nil
}

// validatePart gates one content part, enforcing EXACTLY the single payload the
// discriminator permits so no unproven payload can be admitted and then silently
// dropped by the conversion:
//
//   - "text" admits iff Text is set, valid UTF-8, and NO media payload is also
//     present (a text part carrying an image is a mixed-payload decline);
//   - "output_format" admits iff it carries NO payload field at all;
//   - every media kind declines media_part; anything else declines unknown_part.
func validatePart(msgIdx, partIdx int, p *bamlutils.DynamicContentPart) *Decline {
	mediaSet := p.Image != nil || p.Audio != nil || p.PDF != nil || p.Video != nil
	switch p.Type {
	case "text":
		if p.Text == nil {
			return declinef(StageMessage, ReasonUnknownPart, "message %d part %d is a text part with no text", msgIdx, partIdx)
		}
		if mediaSet {
			return declinef(StageMessage, ReasonMixedPayload, "message %d part %d is a text part carrying an extra media payload", msgIdx, partIdx)
		}
		if !utf8.ValidString(*p.Text) {
			return declinef(StageMessage, ReasonInvalidUTF8, "message %d part %d text is not valid UTF-8", msgIdx, partIdx)
		}
		return nil
	case "output_format":
		if p.Text != nil || mediaSet {
			return declinef(StageMessage, ReasonMixedPayload, "message %d part %d is an output_format part carrying an extra payload", msgIdx, partIdx)
		}
		return nil
	case "image", "audio", "pdf", "video":
		return declinef(StageMessage, ReasonMediaPart, "message %d part %d is a media part outside the proven text-only claim", msgIdx, partIdx)
	default:
		return declinef(StageMessage, ReasonUnknownPart, "message %d part %d has an unproven part type", msgIdx, partIdx)
	}
}

// toNativeMessages converts the GENERATED dynamic messages into the neutral
// native request the prompt renderer consumes. It is a faithful structural
// conversion (roles, text/output_format parts, media, and cache-control
// metadata) so an unproven shape reaches — and is declined by — the native
// renderer/body builder instead of being silently dropped. validateMessages
// has already gated the admitted surface; this conversion stays total so a
// negative shape still round-trips into a genuine downstream decline.
func toNativeMessages(msgs []bamlutils.DynamicMessage) []nativeprompt.Message {
	out := make([]nativeprompt.Message, 0, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		nm := nativeprompt.Message{Role: m.Role}
		if m.Metadata != nil && m.Metadata.CacheControl != nil {
			nm.Metadata = &nativeprompt.Metadata{
				CacheControl: &nativeprompt.CacheControl{Type: m.Metadata.CacheControl.Type},
			}
		}
		if len(m.PartsContent) > 0 {
			nm.Parts = make([]nativeprompt.ContentPart, 0, len(m.PartsContent))
			for j := range m.PartsContent {
				nm.Parts = append(nm.Parts, toNativePart(&m.PartsContent[j]))
			}
		} else if m.TextContent != nil {
			nm.Content = m.TextContent
		}
		out = append(out, nm)
	}
	return out
}

func toNativePart(p *bamlutils.DynamicContentPart) nativeprompt.ContentPart {
	switch p.Type {
	case "text":
		return nativeprompt.ContentPart{Text: p.Text}
	case "output_format":
		return nativeprompt.ContentPart{OutputFormat: true}
	case "image":
		return nativeprompt.ContentPart{Image: toNativeMedia(nativeprompt.MediaImage, p.Image)}
	case "audio":
		return nativeprompt.ContentPart{Audio: toNativeMedia(nativeprompt.MediaAudio, p.Audio)}
	case "pdf":
		return nativeprompt.ContentPart{PDF: toNativeMedia(nativeprompt.MediaPDF, p.PDF)}
	case "video":
		return nativeprompt.ContentPart{Video: toNativeMedia(nativeprompt.MediaVideo, p.Video)}
	default:
		return nativeprompt.ContentPart{}
	}
}

func toNativeMedia(kind nativeprompt.MediaKind, in *bamlutils.MediaInput) *nativeprompt.Media {
	if in == nil {
		return nil
	}
	med := &nativeprompt.Media{Kind: kind}
	if in.URL != nil {
		med.URL = *in.URL
	}
	if in.Base64 != nil {
		med.Base64 = *in.Base64
	}
	if in.MediaType != nil {
		med.Mime = *in.MediaType
	}
	return med
}
