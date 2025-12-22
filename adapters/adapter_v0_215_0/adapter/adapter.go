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

	fmt.Printf("Got type builder: %T %+v", tb, tb)

	for idx, input := range typeBuilder.BamlSnippets {
		if err := tb.AddBaml(input); err != nil {
			return fmt.Errorf("failed to add input at index %d: %w", idx, err)
		}
	}

	b.TypeBuilder = tb

	return nil
}

var _ bamlutils.Adapter = (*BamlAdapter)(nil)
