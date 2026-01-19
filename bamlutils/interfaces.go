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

// RawCollectionMode controls how raw LLM responses are collected
type RawCollectionMode int

const (
	// RawCollectionNone - no raw collection, uses BAML's native streaming
	RawCollectionNone RawCollectionMode = iota
	// RawCollectionFinalOnly - collect raw but skip intermediate parsing (for /call-with-raw)
	RawCollectionFinalOnly
	// RawCollectionAll - full raw collection with intermediate parsing (for /stream-with-raw)
	RawCollectionAll
)

type StreamingPrompt func(adapter Adapter, input any) (<-chan StreamResult, error)

type StreamingMethod struct {
	MakeInput  func() any
	MakeOutput func() any
	Impl       StreamingPrompt
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
	// SetRawCollectionMode sets how raw LLM responses are collected.
	// - RawCollectionNone: uses BAML's native streaming without raw collection
	// - RawCollectionFinalOnly: collects raw but skips intermediate parsing (for /call-with-raw)
	// - RawCollectionAll: full raw collection with intermediate parsing (for /stream-with-raw)
	SetRawCollectionMode(mode RawCollectionMode)
	// RawCollectionMode returns the current raw collection mode.
	RawCollectionMode() RawCollectionMode
}

// BamlOptions contains optional configuration for BAML method calls
type BamlOptions struct {
	ClientRegistry *ClientRegistry `json:"client_registry"`
	TypeBuilder    *TypeBuilder    `json:"type_builder"`
}
