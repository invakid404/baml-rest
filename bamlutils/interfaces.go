package bamlutils

import (
	"context"
)

type StreamResult interface {
	Kind() StreamResultKind
	Stream() any
	Final() any
	Error() error
	// Raw returns the raw LLM response text at this streaming point
	Raw() string
	// Reset returns true if the client should discard accumulated state (retry occurred)
	Reset() bool
	// Release returns the StreamResult to a pool for reuse.
	// After calling Release, the StreamResult should not be accessed.
	Release()
}

type StreamResultKind int

const (
	StreamResultKindStream StreamResultKind = iota
	StreamResultKindFinal
	StreamResultKindError
	StreamResultKindHeartbeat
)

// StreamMode controls how streaming results are processed and what data is collected.
type StreamMode int

const (
	// StreamModeCall - final only, no raw, no partials (for /call endpoint)
	StreamModeCall StreamMode = iota
	// StreamModeStream - partials + final, no raw (for /stream endpoint)
	StreamModeStream
	// StreamModeCallWithRaw - final + raw, no partials (for /call-with-raw endpoint)
	StreamModeCallWithRaw
	// StreamModeStreamWithRaw - partials + final + raw (for /stream-with-raw endpoint)
	StreamModeStreamWithRaw
)

// NeedsRaw returns true if this mode requires raw LLM response collection.
func (m StreamMode) NeedsRaw() bool {
	return m == StreamModeCallWithRaw || m == StreamModeStreamWithRaw
}

// NeedsPartials returns true if this mode requires forwarding partial/intermediate results.
func (m StreamMode) NeedsPartials() bool {
	return m == StreamModeStream || m == StreamModeStreamWithRaw
}

type StreamingPrompt func(adapter Adapter, input any) (<-chan StreamResult, error)

type StreamingMethod struct {
	MakeInput        func() any
	MakeOutput       func() any
	MakeStreamOutput func() any // Stream/partial type (may differ from final output type)
	Impl             StreamingPrompt
}

type ParsePrompt func(adapter Adapter, raw string) (any, error)

type ParseMethod struct {
	MakeOutput func() any
	Impl       ParsePrompt
}

type ClientRegistry struct {
	Primary *string           `json:"primary"`
	Clients []*ClientProperty `json:"clients"`
}

type ClientProperty struct {
	Name        string         `json:"name"`
	Provider    string         `json:"provider"`
	RetryPolicy *string        `json:"retry_policy"`
	Options     map[string]any `json:"options,omitempty"`
}

type TypeBuilder struct {
	BamlSnippets []string `json:"baml_snippets,omitempty"`
}

func (b *TypeBuilder) Add(input string) {
	b.BamlSnippets = append(b.BamlSnippets, input)
}

type BamlTypeBuilder interface {
	AddBaml(string) error
}

type Adapter interface {
	context.Context
	SetClientRegistry(clientRegistry *ClientRegistry) error
	SetTypeBuilder(typeBuilder *TypeBuilder) error
	// SetStreamMode sets the streaming mode which controls partial forwarding and raw collection.
	SetStreamMode(mode StreamMode)
	// StreamMode returns the current streaming mode.
	StreamMode() StreamMode
}

// BamlOptions contains optional configuration for BAML method calls
type BamlOptions struct {
	ClientRegistry *ClientRegistry `json:"client_registry"`
	TypeBuilder    *TypeBuilder    `json:"type_builder"`
}
