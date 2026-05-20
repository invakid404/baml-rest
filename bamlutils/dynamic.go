package bamlutils

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/cloudwego/gjson"
)

// Dynamic endpoint constants
const (
	// DynamicMethodName is the internal BAML method name for dynamic prompts
	DynamicMethodName = "Baml_Rest_Dynamic"
	// DynamicEndpointName is the URL path segment for dynamic endpoints
	DynamicEndpointName = "_dynamic"
)

// CacheControl represents Anthropic prompt caching metadata
type CacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// MessageMetadata contains optional metadata for a message.
// Currently only supports cache_control, but structured for future extensibility.
type MessageMetadata struct {
	CacheControl *CacheControl `json:"cache_control,omitempty"`
	// Future metadata fields go here
}

// DynamicContentPart represents a single part within a multi-part message.
// Used in the user-facing API where content is an array of typed parts.
//
// Each part has a "type" discriminator and a corresponding payload field:
//   - {"type": "text", "text": "..."}
//   - {"type": "image", "image": {"url": "...", "media_type": "..."}}
//   - {"type": "audio", "audio": {"base64": "...", "media_type": "..."}}
//   - {"type": "pdf", "pdf": {"base64": "...", "media_type": "..."}}
//   - {"type": "video", "video": {"url": "...", "media_type": "..."}}
//   - {"type": "output_format"}
type DynamicContentPart struct {
	Type  string      `json:"type"`
	Text  *string     `json:"text,omitempty"`
	Image *MediaInput `json:"image,omitempty"`
	Audio *MediaInput `json:"audio,omitempty"`
	PDF   *MediaInput `json:"pdf,omitempty"`
	Video *MediaInput `json:"video,omitempty"`
}

// payloadFieldNames returns the names of all payload fields that are set (non-nil).
func (p *DynamicContentPart) payloadFieldNames() []string {
	var names []string
	if p.Text != nil {
		names = append(names, "text")
	}
	if p.Image != nil {
		names = append(names, "image")
	}
	if p.Audio != nil {
		names = append(names, "audio")
	}
	if p.PDF != nil {
		names = append(names, "pdf")
	}
	if p.Video != nil {
		names = append(names, "video")
	}
	return names
}

// Validate checks that the content part has a valid type, the required payload,
// and no extraneous payload fields. Exactly one payload field must be set and it
// must match the declared type (output_format must have zero payload fields).
func (p *DynamicContentPart) Validate(msgIdx, partIdx int) error {
	prefix := fmt.Sprintf("message[%d].content[%d]", msgIdx, partIdx)
	set := p.payloadFieldNames()

	switch p.Type {
	case "text":
		if p.Text == nil {
			return fmt.Errorf("%s: text part requires 'text' field", prefix)
		}
	case "image":
		if p.Image == nil {
			return fmt.Errorf("%s: image part requires 'image' field", prefix)
		}
		if err := p.Image.Validate(); err != nil {
			return fmt.Errorf("%s: image %w", prefix, err)
		}
	case "audio":
		if p.Audio == nil {
			return fmt.Errorf("%s: audio part requires 'audio' field", prefix)
		}
		if err := p.Audio.Validate(); err != nil {
			return fmt.Errorf("%s: audio %w", prefix, err)
		}
	case "pdf":
		if p.PDF == nil {
			return fmt.Errorf("%s: pdf part requires 'pdf' field", prefix)
		}
		if err := p.PDF.Validate(); err != nil {
			return fmt.Errorf("%s: pdf %w", prefix, err)
		}
	case "video":
		if p.Video == nil {
			return fmt.Errorf("%s: video part requires 'video' field", prefix)
		}
		if err := p.Video.Validate(); err != nil {
			return fmt.Errorf("%s: video %w", prefix, err)
		}
	case "output_format":
		if len(set) > 0 {
			return fmt.Errorf("%s: output_format part must not have payload fields, got %v", prefix, set)
		}
	default:
		return fmt.Errorf("%s: unknown content part type %q", prefix, p.Type)
	}

	// For non-output_format types: exactly one payload field must be set and match the type
	if p.Type != "output_format" {
		if len(set) > 1 {
			return fmt.Errorf("%s: %s part must have only its own payload field, got %v", prefix, p.Type, set)
		}
		// len(set)==1 is guaranteed here since the switch above verified the matching field is non-nil,
		// but verify the single field matches the type (e.g. reject type:"text" with only "image" set —
		// impossible via JSON since the switch already checked, but guards against future refactors).
		if set[0] != p.Type {
			return fmt.Errorf("%s: %s part has mismatched payload field %q", prefix, p.Type, set[0])
		}
	}

	return nil
}

// DynamicMessage represents a chat message with role, content, and optional metadata.
//
// Content can be either:
//   - A plain string (backward compatible): {"role": "user", "content": "hello"}
//   - An array of content parts (multi-part with media): {"role": "user", "content": [...parts]}
//
// When content is a string, the legacy {output_format} placeholder replacement is supported.
// When content is a parts array, use {"type": "output_format"} as a part instead.
type DynamicMessage struct {
	Role     string           `json:"role"`
	Metadata *MessageMetadata `json:"metadata,omitempty"`

	// Parsed content - exactly one of these will be set after unmarshaling
	TextContent  *string              // Set when content is a plain string
	PartsContent []DynamicContentPart // Set when content is an array of parts
}

// UnmarshalJSON implements custom JSON unmarshaling to handle the content union type.
func (m *DynamicMessage) UnmarshalJSON(data []byte) error {
	// Use a raw type to avoid infinite recursion
	var raw struct {
		Role     string             `json:"role"`
		Content  stdjson.RawMessage `json:"content"`
		Metadata *MessageMetadata   `json:"metadata,omitempty"`
	}
	if err := sonic.Unmarshal(data, &raw); err != nil {
		return err
	}

	m.Role = raw.Role
	m.Metadata = raw.Metadata

	if len(raw.Content) == 0 {
		return nil
	}

	// Determine if content is a string or array
	firstByte := raw.Content[0]
	switch firstByte {
	case '"':
		// String content
		var s string
		if err := sonic.Unmarshal(raw.Content, &s); err != nil {
			return fmt.Errorf("invalid string content: %w", err)
		}
		m.TextContent = &s
	case '[':
		// Array of content parts
		var parts []DynamicContentPart
		if err := sonic.Unmarshal(raw.Content, &parts); err != nil {
			return fmt.Errorf("invalid content parts array: %w", err)
		}
		m.PartsContent = parts
	default:
		return fmt.Errorf("content must be a string or array, got %q", string(raw.Content))
	}

	return nil
}

// MarshalJSON implements custom JSON marshaling.
func (m *DynamicMessage) MarshalJSON() ([]byte, error) {
	type alias struct {
		Role     string           `json:"role"`
		Content  any              `json:"content,omitempty"`
		Parts    any              `json:"parts,omitempty"`
		Metadata *MessageMetadata `json:"metadata,omitempty"`
	}

	a := alias{
		Role:     m.Role,
		Metadata: m.Metadata,
	}

	if m.PartsContent != nil {
		a.Content = m.PartsContent
	} else if m.TextContent != nil {
		a.Content = *m.TextContent
	}

	return sonic.Marshal(a)
}

// internalContentPart is the BAML-facing format for content parts.
// This maps to the Baml_Rest_ContentPart class in dynamic.baml.
// Field names use shortened forms (img, aud, doc, vid) to avoid
// clashing with BAML type keywords (image, audio, pdf, video).
// Media fields are serialized as MediaInput objects which the codegen
// mirror struct mechanism will convert to BAML media types.
type internalContentPart struct {
	Text         *string     `json:"text,omitempty"`
	Img          *MediaInput `json:"img,omitempty"`
	Aud          *MediaInput `json:"aud,omitempty"`
	Doc          *MediaInput `json:"doc,omitempty"`
	Vid          *MediaInput `json:"vid,omitempty"`
	OutputFormat *bool       `json:"output_format,omitempty"`
}

// internalCacheControl is the BAML-facing cache-control struct. The
// generated dynamic BAML input type uses the field name `cache_type`
// (because `type` collides with the BAML type keyword), so we cannot
// reuse the public CacheControl struct here — its `type` JSON tag would
// be dropped when Go JSON unmarshals into the generated input. See #304.
type internalCacheControl struct {
	CacheType string `json:"cache_type"`
}

// internalMessageMetadata is the BAML-facing per-message metadata struct.
// Wire shape matches the generated dynamic input type so JSON round-trips
// preserve cache_control.
type internalMessageMetadata struct {
	CacheControl *internalCacheControl `json:"cache_control,omitempty"`
}

// internalMessage is the BAML-facing format for messages.
// This maps to the Baml_Rest_Message class in dynamic.baml.
type internalMessage struct {
	Role     string                   `json:"role"`
	Content  *string                  `json:"content,omitempty"`
	Parts    []internalContentPart    `json:"parts,omitempty"`
	Metadata *internalMessageMetadata `json:"metadata,omitempty"`
}

// toInternalMessage converts a user-facing DynamicMessage to the internal format.
func (m *DynamicMessage) toInternalMessage() internalMessage {
	msg := internalMessage{Role: m.Role}
	if m.Metadata != nil && m.Metadata.CacheControl != nil {
		msg.Metadata = &internalMessageMetadata{
			CacheControl: &internalCacheControl{
				CacheType: m.Metadata.CacheControl.Type,
			},
		}
	}

	if m.PartsContent != nil {
		parts := make([]internalContentPart, 0, len(m.PartsContent))
		for _, p := range m.PartsContent {
			part := internalContentPart{}
			switch p.Type {
			case "text":
				part.Text = p.Text
			case "image":
				part.Img = p.Image
			case "audio":
				part.Aud = p.Audio
			case "pdf":
				part.Doc = p.PDF
			case "video":
				part.Vid = p.Video
			case "output_format":
				t := true
				part.OutputFormat = &t
			}
			parts = append(parts, part)
		}
		msg.Parts = parts
	} else if m.TextContent != nil {
		msg.Content = m.TextContent
	}

	return msg
}

// DynamicOutputSchema defines the output structure for dynamic endpoints.
// It supports both simple flat schemas and complex nested structures.
//
// Simple flat schema:
//
//	{
//	  "properties": {
//	    "name": {"type": "string"},
//	    "age": {"type": "int"}
//	  }
//	}
//
// Nested structures with classes and enums:
//
//	{
//	  "classes": {
//	    "Address": {
//	      "properties": {
//	        "street": {"type": "string"},
//	        "city": {"type": "string"}
//	      }
//	    }
//	  },
//	  "enums": {
//	    "Status": {
//	      "values": [{"name": "ACTIVE"}, {"name": "INACTIVE"}]
//	    }
//	  },
//	  "properties": {
//	    "name": {"type": "string"},
//	    "address": {"ref": "Address"},
//	    "status": {"ref": "Status"}
//	  }
//	}
type DynamicOutputSchema struct {
	// Properties defines the fields of the output object (required).
	// Insertion order is preserved end-to-end so preserve_schema_order
	// callers do not need a side-channel order slice.
	Properties OrderedMap[*DynamicProperty] `json:"properties"`
	// Classes defines additional class types that can be referenced via ref (optional)
	Classes OrderedMap[*DynamicClass] `json:"classes,omitempty,omitzero"`
	// Enums defines enum types that can be referenced via ref (optional)
	Enums OrderedMap[*DynamicEnum] `json:"enums,omitempty,omitzero"`
}

// MarshalJSON emits the schema as a JSON object, including Classes and
// Enums only when non-empty. The explicit method guarantees the
// empty `"classes":{}` / `"enums":{}` shapes are suppressed regardless
// of the encoder's `omitzero` support.
func (s DynamicOutputSchema) MarshalJSON() ([]byte, error) {
	type alias struct {
		Properties OrderedMap[*DynamicProperty] `json:"properties"`
		Classes    *OrderedMap[*DynamicClass]   `json:"classes,omitempty"`
		Enums      *OrderedMap[*DynamicEnum]    `json:"enums,omitempty"`
	}
	a := alias{Properties: s.Properties}
	if s.Classes.Len() > 0 {
		c := s.Classes
		a.Classes = &c
	}
	if s.Enums.Len() > 0 {
		e := s.Enums
		a.Enums = &e
	}
	return sonic.Marshal(a)
}

// UnmarshalJSON decodes a DynamicOutputSchema. Wire-order is preserved
// via OrderedMap. Duplicate keys in any of the inner objects are
// rejected with a path-qualified error.
func (s *DynamicOutputSchema) UnmarshalJSON(data []byte) error {
	// Reset on reuse: json.Unmarshal into a previously-populated
	// receiver is valid Go usage, and the wire shape carries no
	// caller-managed state we'd need to merge.
	*s = DynamicOutputSchema{}
	if err := checkUniqueTopLevelKeys(data, "output_schema"); err != nil {
		return err
	}
	var raw struct {
		Properties stdjson.RawMessage `json:"properties"`
		Classes    stdjson.RawMessage `json:"classes"`
		Enums      stdjson.RawMessage `json:"enums"`
	}
	if err := sonic.Unmarshal(data, &raw); err != nil {
		return err
	}

	if err := rejectNonObject("output_schema.properties", raw.Properties); err != nil {
		return err
	}
	if isJSONObject(raw.Properties) {
		props, err := unmarshalOrderedMap[*DynamicProperty](raw.Properties, "output_schema.properties")
		if err != nil {
			return err
		}
		s.Properties = props
	}
	if err := rejectNonObject("output_schema.classes", raw.Classes); err != nil {
		return err
	}
	if isJSONObject(raw.Classes) {
		classes, err := unmarshalOrderedMap[*DynamicClass](raw.Classes, "output_schema.classes")
		if err != nil {
			return err
		}
		s.Classes = classes
	}
	if err := rejectNonObject("output_schema.enums", raw.Enums); err != nil {
		return err
	}
	if isJSONObject(raw.Enums) {
		enums, err := unmarshalOrderedMap[*DynamicEnum](raw.Enums, "output_schema.enums")
		if err != nil {
			return err
		}
		s.Enums = enums
	}
	return nil
}

// UnmarshalJSON decodes a DynamicClass preserving the JSON key order of
// its Properties map. Duplicate property keys are rejected.
func (c *DynamicClass) UnmarshalJSON(data []byte) error {
	*c = DynamicClass{}
	if err := checkUniqueTopLevelKeys(data, "class"); err != nil {
		return err
	}
	var raw struct {
		Description string             `json:"description,omitempty"`
		Alias       string             `json:"alias,omitempty"`
		Properties  stdjson.RawMessage `json:"properties,omitempty"`
	}
	if err := sonic.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.Description = raw.Description
	c.Alias = raw.Alias
	if err := rejectNonObject("class.properties", raw.Properties); err != nil {
		return err
	}
	if isJSONObject(raw.Properties) {
		props, err := unmarshalOrderedMap[*DynamicProperty](raw.Properties, "class.properties")
		if err != nil {
			return err
		}
		c.Properties = props
	}
	return nil
}

func isJSONObject(b []byte) bool {
	t := bytes.TrimSpace(b)
	return len(t) > 0 && t[0] == '{'
}

// rejectNonObject returns a path-qualified error when b carries a
// present-but-not-object JSON value. Absent (empty RawMessage) and
// explicit null are accepted to match standard map decoding (an
// absent key or null is the conventional nil-map sentinel).
func rejectNonObject(path string, b []byte) error {
	t := bytes.TrimSpace(b)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) || t[0] == '{' {
		return nil
	}
	return fmt.Errorf("%s: must be a JSON object", path)
}

// checkUniqueTopLevelKeys token-walks the outer object and rejects any
// duplicate key. The struct-tag unmarshal path on the receiver
// (DynamicOutputSchema, DynamicClass) accepts duplicate top-level keys
// with last-wins semantics, contradicting the strict-duplicate rule
// enforced for inner schema objects via unmarshalOrderedObject. Calling
// this before the raw-struct decode restores symmetry — both layers now
// reject ambiguous repeats.
//
// Non-object inputs are left to the regular unmarshal so the caller
// keeps producing the same shape-error it does today.
//
// This is one of two locked stdlib `encoding/json` token-walking sites
// (the other is unmarshalOrderedMap). Sonic does not expose
// Decoder.Token / json.Delim, so duplicate-key rejection over a
// streaming token walk stays on the stdlib decoder.
func checkUniqueTopLevelKeys(data []byte, path string) error {
	dec := stdjson.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	delim, ok := tok.(stdjson.Delim)
	if !ok || delim != '{' {
		return nil
	}
	seen := make(map[string]struct{})
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("%s: expected object key", path)
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%s: duplicate key %q", path, key)
		}
		seen[key] = struct{}{}
		var raw stdjson.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return fmt.Errorf("%s.%s: %w", path, key, err)
		}
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// DynamicInput is the request body for dynamic endpoints
type DynamicInput struct {
	Messages       []DynamicMessage     `json:"messages"`
	ClientRegistry *ClientRegistry      `json:"client_registry"`
	OutputSchema   *DynamicOutputSchema `json:"output_schema"`
	// PreserveSchemaOrder is a tri-state opt-in: nil means "inherit the
	// server default" (BAML_REST_PRESERVE_SCHEMA_ORDER_DEFAULT on the
	// serve host), while a non-nil pointer wins over any server default.
	// JSON null decodes to nil through standard Go pointer unmarshal
	// behavior, matching the absent/inherit semantics.
	PreserveSchemaOrder *bool `json:"preserve_schema_order,omitempty"`
}

// preserveSchemaOrderEnabled resolves the *bool tri-state to a concrete
// boolean for internal use. nil and *false both mean "do not preserve".
func preserveSchemaOrderEnabled(p *bool) bool {
	return p != nil && *p
}

// Validate checks that required fields are present
func (d *DynamicInput) Validate() error {
	if len(d.Messages) == 0 {
		return fmt.Errorf("messages is required and cannot be empty")
	}
	if d.ClientRegistry == nil || d.ClientRegistry.Primary == nil {
		return fmt.Errorf("client_registry with primary is required")
	}
	if d.OutputSchema == nil || d.OutputSchema.Properties.Len() == 0 {
		return fmt.Errorf("output_schema with at least one property is required")
	}
	if err := validateReservedClassNames(d.OutputSchema); err != nil {
		return err
	}
	for i, m := range d.Messages {
		if m.Role == "" {
			return fmt.Errorf("message[%d] role is required", i)
		}
		if m.TextContent == nil && m.PartsContent == nil {
			return fmt.Errorf("message[%d] content is required", i)
		}
		if m.TextContent != nil && *m.TextContent == "" {
			return fmt.Errorf("message[%d] content is required", i)
		}
		if m.PartsContent != nil {
			if len(m.PartsContent) == 0 {
				return fmt.Errorf("message[%d] content parts array cannot be empty", i)
			}
			for j, p := range m.PartsContent {
				if err := p.Validate(i, j); err != nil {
					return err
				}
			}
		}
		if m.Metadata != nil && m.Metadata.CacheControl != nil && strings.TrimSpace(m.Metadata.CacheControl.Type) == "" {
			return fmt.Errorf("messages[%d].metadata.cache_control.type is required", i)
		}
	}
	return nil
}

// ToWorkerInput converts to the internal format for worker processing.
//
// A defer-recover wraps the marshal step as defense-in-depth: this is
// the public worker boundary, and a panic from any encoder reachable
// here would otherwise crash the host process (the original #324
// failure shape on goccy/go-json). Migrating the reachable custom
// marshalers to sonic removes the known trigger; the recover keeps
// future encoder regressions from re-introducing the same outage.
func (d *DynamicInput) ToWorkerInput() (b []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("dynamic input marshal panic: %v", r)
			b = nil
		}
	}()

	classes, err := buildWorkerClassMap(d.OutputSchema)
	if err != nil {
		return nil, err
	}

	internalMessages := make([]internalMessage, 0, len(d.Messages))
	for _, m := range d.Messages {
		internalMessages = append(internalMessages, m.toInternalMessage())
	}

	dynamicTypes := &DynamicTypes{
		Classes: classes,
		Enums:   d.OutputSchema.Enums.Clone(),
	}
	if preserveSchemaOrderEnabled(d.PreserveSchemaOrder) {
		dynamicTypes.PreserveOrder = true
	}

	internal := map[string]any{
		"messages": internalMessages,
		"__baml_options__": &BamlOptions{
			ClientRegistry: d.ClientRegistry,
			TypeBuilder: &TypeBuilder{
				DynamicTypes: dynamicTypes,
			},
		},
	}
	return sonic.Marshal(internal)
}

// DynamicParseInput is the request body for dynamic parse endpoint
type DynamicParseInput struct {
	Raw          string               `json:"raw"`
	OutputSchema *DynamicOutputSchema `json:"output_schema"`
	// PreserveSchemaOrder mirrors DynamicInput.PreserveSchemaOrder —
	// nil inherits the server default, non-nil wins. See that field's
	// doc comment for the full tri-state contract.
	PreserveSchemaOrder *bool `json:"preserve_schema_order,omitempty"`
}

// Validate checks that required fields are present for parse
func (d *DynamicParseInput) Validate() error {
	if d.Raw == "" {
		return fmt.Errorf("raw is required and cannot be empty")
	}
	if d.OutputSchema == nil || d.OutputSchema.Properties.Len() == 0 {
		return fmt.Errorf("output_schema with at least one property is required")
	}
	if err := validateReservedClassNames(d.OutputSchema); err != nil {
		return err
	}
	return nil
}

// validateReservedClassNames rejects user-supplied class names that
// collide with synthetic names baml-rest writes into the worker
// payload. buildWorkerClassMap unconditionally overwrites the entry
// at dynamicOutputClassName with the synthetic top-level class, so
// without this guard a user class with that exact name would be
// silently dropped.
func validateReservedClassNames(s *DynamicOutputSchema) error {
	if s == nil {
		return nil
	}
	if s.Classes.Has(dynamicOutputClassName) {
		return fmt.Errorf("output_schema.classes: %q is reserved by baml-rest", dynamicOutputClassName)
	}
	return nil
}

// ToWorkerInput converts to the internal format for worker processing.
// See DynamicInput.ToWorkerInput for the defer-recover rationale.
func (d *DynamicParseInput) ToWorkerInput() (b []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("dynamic parse input marshal panic: %v", r)
			b = nil
		}
	}()

	classes, err := buildWorkerClassMap(d.OutputSchema)
	if err != nil {
		return nil, err
	}

	dynamicTypes := &DynamicTypes{
		Classes: classes,
		Enums:   d.OutputSchema.Enums.Clone(),
	}
	if preserveSchemaOrderEnabled(d.PreserveSchemaOrder) {
		dynamicTypes.PreserveOrder = true
	}

	internal := map[string]any{
		"raw": d.Raw,
		"__baml_options__": &BamlOptions{
			TypeBuilder: &TypeBuilder{
				DynamicTypes: dynamicTypes,
			},
		},
	}
	return sonic.Marshal(internal)
}

// dynamicOutputClassName is the synthetic class name baml-rest assigns
// to the top-level dynamic output. Surfaced as a constant so codegen
// and the order helper agree on where the synthetic class lives in the
// preserved class order.
const dynamicOutputClassName = "Baml_Rest_DynamicOutput"

// buildWorkerClassMap collects user-defined classes and injects the
// synthetic Baml_Rest_DynamicOutput class first, mirroring the top-level
// output_schema.properties. The synthetic class is placed at the front
// so preserve_order callers see it ahead of user classes regardless of
// alphabetical ordering. Reserved-name overlap is rejected upstream by
// validateReservedClassNames; if a programming error slips through (or
// a caller hand-builds the worker payload), the duplicate Set surfaces
// here as an error rather than a silent overwrite.
func buildWorkerClassMap(schema *DynamicOutputSchema) (OrderedMap[*DynamicClass], error) {
	var classes OrderedMap[*DynamicClass]
	if err := classes.Set(dynamicOutputClassName, &DynamicClass{
		Properties: schema.Properties.Clone(),
	}); err != nil {
		return OrderedMap[*DynamicClass]{}, err
	}
	var outErr error
	schema.Classes.Range(func(name string, class *DynamicClass) bool {
		if err := classes.Set(name, class); err != nil {
			outErr = err
			return false
		}
		return true
	})
	if outErr != nil {
		return OrderedMap[*DynamicClass]{}, outErr
	}
	return classes, nil
}

// FlattenDynamicOutput extracts the DynamicProperties field from a dynamic endpoint response.
//
// Input:  {"DynamicProperties": {"name": "John", "age": 30}}
// Output: {"name": "John", "age": 30}
//
// If the input doesn't have a DynamicProperties field, it's returned as-is.
func FlattenDynamicOutput(data []byte) ([]byte, error) {
	dynProps := gjson.GetBytes(data, "DynamicProperties")
	if !dynProps.Exists() {
		return data, nil
	}
	return []byte(dynProps.Raw), nil
}
