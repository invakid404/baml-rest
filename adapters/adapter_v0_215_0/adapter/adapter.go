package adapter

import (
	"context"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/introspected"
)

// TypeBuilderFactory creates a new TypeBuilder and applies the TypeBuilder config.
// The generated code implements this to have direct access to the generated
// TypeBuilder methods for processing DynamicTypes and BamlSnippets.
type TypeBuilderFactory func(tb *bamlutils.TypeBuilder) (*introspected.TypeBuilder, error)

// MediaFactory creates BAML media objects (image, audio, pdf, video) from URL or base64 data.
// Populated by the generated code which has access to the baml_client package-level constructors.
type MediaFactory func(kind bamlutils.MediaKind, url *string, base64 *string, mimeType *string) (any, error)

type BamlAdapter struct {
	context.Context

	TypeBuilderFactory TypeBuilderFactory
	MediaFactory       MediaFactory

	ClientRegistry *baml.ClientRegistry
	TypeBuilder    *introspected.TypeBuilder

	// streamMode controls how streaming results are processed.
	streamMode bamlutils.StreamMode

	// logger is used for debug output during dynamic type processing.
	logger bamlutils.Logger
}

func (b *BamlAdapter) SetClientRegistry(clientRegistry *bamlutils.ClientRegistry) error {
	b.ClientRegistry = baml.NewClientRegistry()

	for _, client := range clientRegistry.Clients {
		// BAML 0.215.0+ properly handles nested maps, no WrapMapValues needed
		b.ClientRegistry.AddLlmClient(client.Name, client.Provider, client.Options)
	}

	if clientRegistry.Primary != nil {
		b.ClientRegistry.SetPrimaryClient(*clientRegistry.Primary)
	}

	return nil
}

func (b *BamlAdapter) SetTypeBuilder(tb *bamlutils.TypeBuilder) error {
	typeBuilder, err := b.TypeBuilderFactory(tb)
	if err != nil {
		return err
	}
	b.TypeBuilder = typeBuilder
	return nil
}

func (b *BamlAdapter) SetStreamMode(mode bamlutils.StreamMode) {
	b.streamMode = mode
}

func (b *BamlAdapter) StreamMode() bamlutils.StreamMode {
	return b.streamMode
}

func (b *BamlAdapter) SetLogger(logger bamlutils.Logger) {
	b.logger = logger
}

func (b *BamlAdapter) Logger() bamlutils.Logger {
	return b.logger
}

func (b *BamlAdapter) NewMediaFromURL(kind bamlutils.MediaKind, url string, mimeType *string) (any, error) {
	return b.MediaFactory(kind, &url, nil, mimeType)
}

func (b *BamlAdapter) NewMediaFromBase64(kind bamlutils.MediaKind, base64 string, mimeType *string) (any, error) {
	return b.MediaFactory(kind, nil, &base64, mimeType)
}

var _ bamlutils.Adapter = (*BamlAdapter)(nil)
