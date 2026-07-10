package nativeprompt

import (
	stdjson "encoding/json"
	"fmt"
	"strings"
)

// lower reconstructs the structured RenderedPrompt from the rendered template
// string, reproducing BAML v0.223's post-render split
// (engine/baml-lib/jinja-runtime/src/lib.rs):
//
//   - if the output contains neither the role nor the media delimiter, it is a
//     Completion;
//   - otherwise split on the role delimiter: chunks that are role markers open
//     a new message; every other chunk is a message body that is split on the
//     media delimiter into text parts (trimmed, empties dropped) and media
//     parts.
func lower(rendered string) (*RenderedPrompt, error) {
	if !strings.Contains(rendered, roleDelim) && !strings.Contains(rendered, mediaDelim) {
		return &RenderedPrompt{Kind: KindCompletion, Completion: rendered}, nil
	}

	var messages []RenderedMessage
	curIdx := -1

	for _, chunk := range strings.Split(rendered, roleDelim) {
		if isRoleMarker(chunk) {
			role, allowDupe, meta, err := parseRoleMarker(chunk)
			if err != nil {
				return nil, err
			}
			messages = append(messages, RenderedMessage{
				Role:               role,
				AllowDuplicateRole: allowDupe,
				Meta:               meta,
				Parts:              []Part{},
			})
			curIdx = len(messages) - 1
			continue
		}

		parts, err := parseBody(chunk)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			continue
		}
		if curIdx < 0 {
			// Content before the first role marker. The dynamic template always
			// opens each message with _.role(...), so this never occurs for the
			// supported shape; fail closed rather than guess a default role.
			return nil, fmt.Errorf("nativeprompt: rendered content before first role marker")
		}
		messages[curIdx].Parts = append(messages[curIdx].Parts, parts...)
	}

	return &RenderedPrompt{Kind: KindChat, Messages: messages}, nil
}

func isRoleMarker(chunk string) bool {
	return strings.HasPrefix(chunk, roleMarkerPrefix) && strings.HasSuffix(chunk, roleMarkerSuffix)
}

// parseRoleMarker parses a role-marker chunk (:baml-start-baml:{json}:baml-end-baml:)
// into role, allow-duplicate-role, and the remaining metadata kwargs.
func parseRoleMarker(chunk string) (role string, allowDupe bool, meta map[string]any, err error) {
	inner := strings.TrimSuffix(strings.TrimPrefix(chunk, roleMarkerPrefix), roleMarkerSuffix)

	var raw map[string]any
	if err := stdjson.Unmarshal([]byte(inner), &raw); err != nil {
		return "", false, nil, fmt.Errorf("nativeprompt: parse role marker %q: %w", inner, err)
	}

	if r, ok := raw[roleKey].(string); ok {
		role = r
	} else {
		return "", false, nil, fmt.Errorf("nativeprompt: role marker missing string role: %q", inner)
	}
	if b, ok := raw[allowDupeRoleKey].(bool); ok {
		allowDupe = b
	}

	for k, v := range raw {
		if k == roleKey || k == allowDupeRoleKey {
			continue
		}
		if meta == nil {
			meta = map[string]any{}
		}
		meta[k] = v
	}
	return role, allowDupe, meta, nil
}

// parseBody splits a message body on the media delimiter and reconstructs its
// ordered parts: media markers become MediaParts; every other segment is a text
// part, trimmed, with empty segments dropped.
func parseBody(chunk string) ([]Part, error) {
	var parts []Part
	for _, seg := range strings.Split(chunk, mediaDelim) {
		if isMediaMarker(seg) {
			media, err := parseMediaMarker(seg)
			if err != nil {
				return nil, err
			}
			parts = append(parts, Part{Media: media})
			continue
		}
		trimmed := strings.TrimSpace(seg)
		if trimmed == "" {
			continue
		}
		parts = append(parts, textPart(trimmed))
	}
	return parts, nil
}

func isMediaMarker(seg string) bool {
	return strings.HasPrefix(seg, mediaMarkerPrefix) && strings.HasSuffix(seg, mediaMarkerSuffix)
}

func parseMediaMarker(seg string) (*MediaPart, error) {
	inner := strings.TrimSuffix(strings.TrimPrefix(seg, mediaMarkerPrefix), mediaMarkerSuffix)
	var mp MediaPart
	if err := stdjson.Unmarshal([]byte(inner), &mp); err != nil {
		return nil, fmt.Errorf("nativeprompt: parse media marker %q: %w", inner, err)
	}
	return &mp, nil
}
