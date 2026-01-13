package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/goccy/go-json"
	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	baml_rest "github.com/invakid404/baml-rest"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/memlimit"
	"github.com/invakid404/baml-rest/workerplugin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"google.golang.org/protobuf/proto"
)

func main() {
	// Initialize BAML runtime - this loads the shared library
	baml_rest.InitBamlRuntime()

	// Create hclog logger for go-plugin communication.
	// Logs are routed back to the main process via go-plugin's protocol.
	logger := hclog.New(&hclog.LoggerOptions{
		Level:      hclog.Debug,
		Output:     os.Stderr,
		JSONFormat: true,
	})

	// Start RSS monitor to trigger GC when native memory pressure is high.
	// BAML's native (Rust) memory isn't visible to Go's GC, so we monitor RSS
	// and force GC to run finalizers that clean up native resources.
	//
	// Note: We discard the stop function because workers run until the parent
	// go-plugin process terminates them. There's no graceful shutdown path.
	if memLimitStr := os.Getenv("GOMEMLIMIT"); memLimitStr != "" {
		if memLimit, err := memlimit.ParseBytes(memLimitStr); err == nil && memLimit > 0 {
			// Trigger GC when RSS exceeds 80% of memory limit
			threshold := memLimit * 8 / 10
			_ = memlimit.StartRSSMonitor(memlimit.RSSMonitorConfig{
				Threshold: threshold,
				Interval:  5 * time.Second,
				OnGC: func(rssBefore, rssAfter int64, result memlimit.GCResult) {
					logger.Debug("RSS-triggered GC completed",
						"rss_before", memlimit.FormatBytes(rssBefore),
						"rss_after", memlimit.FormatBytes(rssAfter),
						"threshold", memlimit.FormatBytes(threshold),
						"result", result.String(),
					)
				},
			})
		}
	}

	// Set up Prometheus metrics registry for this worker
	metricsReg := prometheus.NewRegistry()
	metricsReg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: workerplugin.Handshake,
		Plugins: map[string]goplugin.Plugin{
			"worker": &workerplugin.WorkerPlugin{Impl: &workerImpl{metricsReg: metricsReg}},
		},
		GRPCServer: goplugin.DefaultGRPCServer,
		Logger:     logger,
	})
}

// workerImpl implements the workerplugin.Worker interface
type workerImpl struct {
	metricsReg *prometheus.Registry
}

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
			pluginResult := workerplugin.GetStreamResult()

			switch result.Kind() {
			case bamlutils.StreamResultKindError:
				pluginResult.Kind = workerplugin.StreamResultKindError
				pluginResult.Error = result.Error()
			case bamlutils.StreamResultKindStream:
				data, err := json.Marshal(result.Stream())
				if err != nil {
					pluginResult.Kind = workerplugin.StreamResultKindError
					pluginResult.Error = fmt.Errorf("failed to marshal stream result: %w", err)
				} else {
					pluginResult.Kind = workerplugin.StreamResultKindStream
					pluginResult.Data = data
					pluginResult.Raw = result.Raw()
				}
			case bamlutils.StreamResultKindFinal:
				data, err := json.Marshal(result.Final())
				if err != nil {
					pluginResult.Kind = workerplugin.StreamResultKindError
					pluginResult.Error = fmt.Errorf("failed to marshal final result: %w", err)
				} else {
					pluginResult.Kind = workerplugin.StreamResultKindFinal
					pluginResult.Data = data
					pluginResult.Raw = result.Raw()
				}
			case bamlutils.StreamResultKindHeartbeat:
				pluginResult.Kind = workerplugin.StreamResultKindHeartbeat
			}

			// Release the adapter's output struct back to its pool
			result.Release()

			select {
			case out <- pluginResult:
			case <-ctx.Done():
				// Release the plugin result we couldn't send
				workerplugin.ReleaseStreamResult(pluginResult)
				return
			}
		}
	}()

	return out, nil
}

func (w *workerImpl) Health(ctx context.Context) (bool, error) {
	return true, nil
}

func (w *workerImpl) GetMetrics(ctx context.Context) ([][]byte, error) {
	mfs, err := w.metricsReg.Gather()
	if err != nil {
		return nil, fmt.Errorf("failed to gather metrics: %w", err)
	}

	result := make([][]byte, 0, len(mfs))
	for _, mf := range mfs {
		data, err := proto.Marshal(mf)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal metric family %s: %w", mf.GetName(), err)
		}
		result = append(result, data)
	}
	return result, nil
}
