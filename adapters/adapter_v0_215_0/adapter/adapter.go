package adapter

import (
	"context"
	"fmt"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
	"github.com/invakid404/baml-rest/bamlutils"
)

type BamlAdapter struct {
	context.Context

	TypeBuilderFactory func() (bamlutils.BamlTypeBuilder, error)

	ClientRegistry *baml.ClientRegistry
	TypeBuilder    bamlutils.BamlTypeBuilder

	// rawCollectionMode controls how raw LLM responses are collected.
	// - RawCollectionNone: uses BAML's native streaming without raw collection
	// - RawCollectionFinalOnly: collects raw but skips intermediate parsing (for /call-with-raw)
	// - RawCollectionAll: full raw collection with intermediate parsing (for /stream-with-raw)
	rawCollectionMode bamlutils.RawCollectionMode
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

func (b *BamlAdapter) SetTypeBuilder(typeBuilder *bamlutils.TypeBuilder) error {
	tb, err := b.TypeBuilderFactory()
	if err != nil {
		return fmt.Errorf("failed to create type builder: %w", err)
	}

	for idx, input := range typeBuilder.BamlSnippets {
		if err := tb.AddBaml(input); err != nil {
			return fmt.Errorf("failed to add input at index %d: %w", idx, err)
		}
	}

	b.TypeBuilder = tb

	return nil
}

func (b *BamlAdapter) SetRawCollectionMode(mode bamlutils.RawCollectionMode) {
	b.rawCollectionMode = mode
}

func (b *BamlAdapter) RawCollectionMode() bamlutils.RawCollectionMode {
	return b.rawCollectionMode
}

var _ bamlutils.Adapter = (*BamlAdapter)(nil)
