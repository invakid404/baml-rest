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

type StreamingPrompt func(adapter Adapter, input any) (<-chan StreamResult, error)

type StreamingMethod struct {
	MakeInput  func() any
	MakeOutput func() any
	Impl       StreamingPrompt
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
	// SetRawCollection enables or disables raw LLM response collection.
	// When true, the OnTick callback captures raw SSE data for the Raw() method.
	// When false, just uses BAML's native streaming without raw collection overhead.
	SetRawCollection(enabled bool)
	// RawCollectionEnabled returns whether raw LLM response collection is enabled.
	RawCollectionEnabled() bool
}

// BamlOptions contains optional configuration for BAML method calls
type BamlOptions struct {
	ClientRegistry *ClientRegistry `json:"client_registry"`
	TypeBuilder    *TypeBuilder    `json:"type_builder"`
}
