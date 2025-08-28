package baml

import "context"

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

type StreamingPrompt func(ctx context.Context, input any) (<-chan StreamResult, error)

type StreamingMethod struct {
	MakeInput  func() any
	MakeOutput func() any
	Impl       StreamingPrompt
}
