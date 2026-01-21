package adapter

import (
	"context"
	"fmt"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
	"github.com/invakid404/baml-rest/adapters/common/typebuilder"
	"github.com/invakid404/baml-rest/bamlutils"
)

type BamlAdapter struct {
	context.Context

	TypeBuilderFactory func() (bamlutils.BamlTypeBuilder, error)

	ClientRegistry *baml.ClientRegistry
	TypeBuilder    bamlutils.BamlTypeBuilder

	// streamMode controls how streaming results are processed.
	streamMode bamlutils.StreamMode
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
	wrapped, err := b.TypeBuilderFactory()
	if err != nil {
		return fmt.Errorf("failed to create type builder: %w", err)
	}

	// Process dynamic_types first (imperative API)
	if tb.DynamicTypes != nil {
		translator := typebuilder.NewTranslator(wrapped)
		if err := translator.Apply(tb.DynamicTypes); err != nil {
			return fmt.Errorf("failed to apply dynamic types: %w", err)
		}
	}

	// Then add BAML snippets (can reference types created above)
	for idx, input := range tb.BamlSnippets {
		if err := wrapped.AddBaml(input); err != nil {
			return fmt.Errorf("baml_snippets[%d]: %w", idx, err)
		}
	}

	b.TypeBuilder = wrapped

	return nil
}

func (b *BamlAdapter) SetStreamMode(mode bamlutils.StreamMode) {
	b.streamMode = mode
}

func (b *BamlAdapter) StreamMode() bamlutils.StreamMode {
	return b.streamMode
}

var _ bamlutils.Adapter = (*BamlAdapter)(nil)
