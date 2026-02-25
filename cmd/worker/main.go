package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strings"
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
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
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
			"worker": &workerplugin.WorkerPlugin{Impl: &workerImpl{metricsReg: metricsReg, logger: logger}},
		},
		GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
			opts = append(opts,
				grpc.KeepaliveParams(keepalive.ServerParameters{
					Time:    30 * time.Second, // Server pings after 30s idle
					Timeout: 10 * time.Second, // Wait 10s for ack
				}),
				grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
					MinTime:             10 * time.Second, // Must be <= client Time
					PermitWithoutStream: true,             // Allow pings between bursts
				}),
				grpc.NumStreamWorkers(uint32(runtime.NumCPU())),
			)
			return grpc.NewServer(opts...)
		},
		Logger: logger,
	})
}

// workerImpl implements the workerplugin.Worker interface
type workerImpl struct {
	metricsReg *prometheus.Registry
	logger     bamlutils.Logger
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

func (w *workerImpl) CallStream(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
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
	adapter.SetLogger(w.logger)
	adapter.SetStreamMode(streamMode)
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

func (w *workerImpl) TriggerGC(ctx context.Context) (*workerplugin.GCResult, error) {
	// Capture memory stats before GC
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	// Force garbage collection
	runtime.GC()

	// Aggressively release memory to OS
	debug.FreeOSMemory()

	// Capture memory stats after GC
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	return &workerplugin.GCResult{
		HeapAllocBefore: memBefore.HeapAlloc,
		HeapAllocAfter:  memAfter.HeapAlloc,
		HeapReleased:    memAfter.HeapReleased - memBefore.HeapReleased,
	}, nil
}

func (w *workerImpl) GetGoroutines(ctx context.Context, filter string) (*workerplugin.GoroutinesResult, error) {
	// Capture goroutine profile with full stacks
	var buf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 2); err != nil {
		return nil, fmt.Errorf("failed to capture goroutine profile: %w", err)
	}

	stacks := buf.String()
	totalCount := int32(runtime.NumGoroutine())

	result := &workerplugin.GoroutinesResult{
		TotalCount: totalCount,
	}

	// Parse include and exclude patterns (patterns prefixed with - are exclusions)
	var includePatterns, excludePatterns []string
	if filter != "" {
		for _, pattern := range strings.Split(filter, ",") {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				continue
			}
			if strings.HasPrefix(pattern, "-") {
				excludePatterns = append(excludePatterns, strings.ToLower(strings.TrimPrefix(pattern, "-")))
			} else {
				includePatterns = append(includePatterns, strings.ToLower(pattern))
			}
		}
	}

	// If include patterns provided, count matching goroutines (case-insensitive)
	if len(includePatterns) > 0 {
		goroutineStacks := strings.Split(stacks, "goroutine ")
		for _, stack := range goroutineStacks {
			if stack == "" {
				continue
			}
			stackLower := strings.ToLower(stack)

			// Check if stack matches any include pattern
			matched := false
			for _, pattern := range includePatterns {
				if strings.Contains(stackLower, pattern) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}

			// Check if stack matches any exclude pattern
			excluded := false
			for _, pattern := range excludePatterns {
				if strings.Contains(stackLower, pattern) {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}

			result.MatchCount++
			// Truncate for readability
			if len(stack) > 1000 {
				stack = stack[:1000] + "..."
			}
			result.MatchedStacks = append(result.MatchedStacks, "goroutine "+stack)
		}
	}

	return result, nil
}

// workerParseInput wraps the input for parse requests
type workerParseInput struct {
	Raw     string                 `json:"raw"`
	Options *bamlutils.BamlOptions `json:"__baml_options__,omitempty"`
}

func (w *workerImpl) Parse(ctx context.Context, methodName string, inputJSON []byte) (*workerplugin.ParseResult, error) {
	method, ok := baml_rest.ParseMethods[methodName]
	if !ok {
		return nil, fmt.Errorf("parse method %q not found", methodName)
	}

	// Parse input
	var input workerParseInput
	if err := json.Unmarshal(inputJSON, &input); err != nil {
		return nil, fmt.Errorf("failed to unmarshal input: %w", err)
	}

	if input.Raw == "" {
		return nil, fmt.Errorf("missing required field 'raw'")
	}

	// Create adapter and apply options
	adapter := baml_rest.MakeAdapter(ctx)
	adapter.SetLogger(w.logger)
	if input.Options != nil {
		opts := workerBamlOptions{Options: input.Options}
		if err := opts.apply(adapter); err != nil {
			return nil, fmt.Errorf("failed to apply options: %w", err)
		}
	}

	// Call the parse method
	result, err := method.Impl(adapter, input.Raw)
	if err != nil {
		return nil, err
	}

	// Marshal the result to JSON
	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal parse result: %w", err)
	}

	return &workerplugin.ParseResult{Data: data}, nil
}
