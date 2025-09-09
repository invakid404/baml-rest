package bamlutils

import (
	"context"
)

type StreamResult interface {
	Kind() StreamResultKind
	Stream() any
	Final() any
	Error() error
}

type StreamResultKind int

const (
	StreamResultKindStream StreamResultKind = iota
	StreamResultKindFinal
	StreamResultKindError
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

type Adapter interface {
	context.Context
	SetClientRegistry(clientRegistry *ClientRegistry) error
	SetTypeBuilder(typeBuilder *TypeBuilder) error
}
