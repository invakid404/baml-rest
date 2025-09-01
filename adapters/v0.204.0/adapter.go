package v0_204_0

import (
	"context"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
	"github.com/invakid404/baml-rest/bamlutils"
)

type BamlAdapter struct {
	context.Context
	ClientRegistry *baml.ClientRegistry
}

func (b *BamlAdapter) SetClientRegistry(clientRegistry *bamlutils.ClientRegistry) {
	b.ClientRegistry = baml.NewClientRegistry()

	for _, client := range clientRegistry.Clients {
		b.ClientRegistry.AddLlmClient(client.Name, client.Provider, client.Options)
	}

	if clientRegistry.Primary != nil {
		b.ClientRegistry.SetPrimaryClient(*clientRegistry.Primary)
	}
}

var _ bamlutils.Adapter = (*BamlAdapter)(nil)
