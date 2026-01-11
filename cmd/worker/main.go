package main

import (
	"context"
	"fmt"

	"github.com/goccy/go-json"
	goplugin "github.com/hashicorp/go-plugin"
	baml_rest "github.com/invakid404/baml-rest"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

func main() {
	// Initialize BAML runtime - this loads the shared library
	baml_rest.InitBamlRuntime()

	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: workerplugin.Handshake,
		Plugins: map[string]goplugin.Plugin{
			"worker": &workerplugin.WorkerPlugin{Impl: &workerImpl{}},
		},
		GRPCServer: goplugin.DefaultGRPCServer,
	})
}

// workerImpl implements the workerplugin.Worker interface
type workerImpl struct{}

// workerBamlOptions wraps the options for JSON parsing
type workerBamlOptions struct {
	Options *bamlutils.BamlOptions `json:"__baml_options__,omitempty"`
}

func (o *workerBamlOptions) apply(adapter bamlutils.Adapter) error {
	if o.Options == nil {
		return nil
	}

	if o.Options.ClientRegistry != nil {
		if err := adapter.SetClientRegistry(o.Options.ClientRegistry); err != nil {
			return fmt.Errorf("failed to set client registry: %w", err)
		}
	}

	if o.Options.TypeBuilder != nil {
		if err := adapter.SetTypeBuilder(o.Options.TypeBuilder); err != nil {
			return fmt.Errorf("failed to set type builder: %w", err)
		}
	}

	return nil
}

func (w *workerImpl) CallStream(ctx context.Context, methodName string, inputJSON []byte, enableRawCollection bool) (<-chan *workerplugin.StreamResult, error) {
	method, ok := baml_rest.Methods[methodName]
	if !ok {
		return nil, fmt.Errorf("method %q not found", methodName)
	}

	// Parse input (unknown fields like __baml_options__ are ignored)
	input := method.MakeInput()
	if err := json.Unmarshal(inputJSON, input); err != nil {
		return nil, fmt.Errorf("failed to unmarshal input: %w", err)
	}

	// Parse options from the same input (only extracts __baml_options__ field)
	var options workerBamlOptions
	if err := json.Unmarshal(inputJSON, &options); err != nil {
		return nil, fmt.Errorf("failed to unmarshal options: %w", err)
	}

	// Create adapter and apply options
	adapter := baml_rest.MakeAdapter(ctx)
	adapter.SetRawCollection(enableRawCollection)
	if err := options.apply(adapter); err != nil {
		return nil, fmt.Errorf("failed to apply options: %w", err)
	}

	// Execute the method
	resultChan, err := method.Impl(adapter, input)
	if err != nil {
		return nil, fmt.Errorf("failed to call method: %w", err)
	}

	// Convert to plugin stream results
	out := make(chan *workerplugin.StreamResult)
	go func() {
		defer close(out)
		for result := range resultChan {
			var pluginResult *workerplugin.StreamResult

			switch result.Kind() {
			case bamlutils.StreamResultKindError:
				pluginResult = &workerplugin.StreamResult{
					Kind:  workerplugin.StreamResultKindError,
					Error: result.Error(),
				}
			case bamlutils.StreamResultKindStream:
				data, err := json.Marshal(result.Stream())
				if err != nil {
					pluginResult = &workerplugin.StreamResult{
						Kind:  workerplugin.StreamResultKindError,
						Error: fmt.Errorf("failed to marshal stream result: %w", err),
					}
				} else {
					pluginResult = &workerplugin.StreamResult{
						Kind: workerplugin.StreamResultKindStream,
						Data: data,
						Raw:  result.Raw(),
					}
				}
			case bamlutils.StreamResultKindFinal:
				data, err := json.Marshal(result.Final())
				if err != nil {
					pluginResult = &workerplugin.StreamResult{
						Kind:  workerplugin.StreamResultKindError,
						Error: fmt.Errorf("failed to marshal final result: %w", err),
					}
				} else {
					pluginResult = &workerplugin.StreamResult{
						Kind: workerplugin.StreamResultKindFinal,
						Data: data,
						Raw:  result.Raw(),
					}
				}
			}

			select {
			case out <- pluginResult:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}

func (w *workerImpl) Health(ctx context.Context) (bool, error) {
	return true, nil
}
