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
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"
	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	baml_rest "github.com/invakid404/baml-rest"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/clientdefaults"
	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
	"github.com/invakid404/baml-rest/internal/memlimit"
	"github.com/invakid404/baml-rest/workerplugin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"google.golang.org/grpc"
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

	// Load deployment-wide ClientRegistry defaults. A parse failure here is
	// fatal by design: the worker exits non-zero, go-plugin's handshake
	// fails, and pool.New surfaces the error to serve's startup. This keeps
	// misconfiguration loud instead of silently ignoring the env var.
	//
	// logger.Error (hclog-structured JSON on stderr) is the only channel
	// go-plugin preserves; fmt.Fprintln / log.Fatal output would be demoted
	// to debug or dropped entirely by the plugin host.
	var err error
	workerClientDefaults, err = clientdefaults.Load()
	if err != nil {
		logger.Error("invalid BAML_REST_CLIENT_DEFAULTS", "err", err.Error())
		os.Exit(1)
	}
	if workerClientDefaults.HasKey("allowed_role_metadata") && buildrequest.UseBuildRequest() {
		logger.Warn(
			"BAML_REST_CLIENT_DEFAULTS sets allowed_role_metadata but " +
				"BAML_REST_USE_BUILD_REQUEST=true; message-level metadata " +
				"(e.g. cache_control) is dropped by the BuildRequest serializer " +
				"until the upstream TODOs are resolved: " +
				"baml_language/crates/sys_llm/src/build_request/openai.rs:100 and " +
				"baml_language/crates/sys_llm/src/build_request/anthropic.rs:91")
	}

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
			opts = append(opts, workerplugin.GRPCServerOptions()...)
			return grpc.NewServer(opts...)
		},
		Logger: logger,
	})
}

// defaultDrainLeakThreshold is how long a drain goroutine waits before logging
// a warning that the producer has not closed its result channel. This does NOT
// add a hard timeout — the drain still waits for close — but alerts operators
// to a likely leak so they can investigate the misbehaving adapter.
const defaultDrainLeakThreshold = 30 * time.Second

// drainLeakThreshold stores the current threshold in nanoseconds. Tests can
// override it via setDrainLeakThreshold. The atomic avoids data races between
// test goroutines and drain goroutines from earlier tests.
var drainLeakThresholdNs atomic.Int64

func init() {
	drainLeakThresholdNs.Store(int64(defaultDrainLeakThreshold))
}

func getDrainLeakThreshold() time.Duration {
	return time.Duration(drainLeakThresholdNs.Load())
}

func setDrainLeakThreshold(d time.Duration) {
	drainLeakThresholdNs.Store(int64(d))
}

// activeDrainGoroutines tracks how many drain goroutines are currently running.
// Exported for operational monitoring (e.g. Prometheus gauge, debug endpoint).
var activeDrainGoroutines atomic.Int64

// ActiveDrainGoroutines returns the number of drain goroutines currently
// waiting for a producer to close its result channel.
func ActiveDrainGoroutines() int64 {
	return activeDrainGoroutines.Load()
}

// workerClientDefaults holds deployment-wide ClientRegistry option defaults
// parsed from BAML_REST_CLIENT_DEFAULTS at worker startup. Always non-nil
// after main() runs; Apply is a no-op when no options were configured.
var workerClientDefaults *clientdefaults.Config

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
		// Apply URL rewrite rules to custom client base_url options
		if rules := urlrewrite.GlobalRules(); len(rules) > 0 {
			rewriteClientBaseURLs(o.Options.ClientRegistry, rules)
		}
		// Merge deployment-wide defaults *after* URL rewrites (so injected
		// values aren't accidentally URL-rewritten) and *before*
		// SetClientRegistry (so BAML sees the merged options).
		workerClientDefaults.Apply(o.Options.ClientRegistry)
		if err := adapter.SetClientRegistry(o.Options.ClientRegistry); err != nil {
			return fmt.Errorf("failed to set client registry: %w", err)
		}
	}

	if o.Options.TypeBuilder != nil {
		if err := adapter.SetTypeBuilder(o.Options.TypeBuilder); err != nil {
			return fmt.Errorf("failed to set type builder: %w", err)
		}
	}

	if o.Options.Retry != nil {
		adapter.SetRetryConfig(o.Options.Retry)
	}

	// Always pass IncludeThinkingInRaw through (even when false) so the
	// adapter reflects an explicit per-request choice. Default value
	// matches BAML's RawLLMResponse() text-only contract.
	adapter.SetIncludeThinkingInRaw(o.Options.IncludeThinkingInRaw)

	return nil
}

// rewriteClientBaseURLs applies URL rewrite rules to the base_url option
// of each client in the registry. This allows remapping external URLs
// (e.g., https://llm.mandel.ai) to internal URLs (e.g., http://litellm:4000)
// for custom clients passed via __baml_options__.
func rewriteClientBaseURLs(registry *bamlutils.ClientRegistry, rules []urlrewrite.Rule) {
	for _, client := range registry.Clients {
		if client == nil || client.Options == nil {
			continue
		}
		baseURL, ok := client.Options["base_url"]
		if !ok {
			continue
		}
		urlStr, ok := baseURL.(string)
		if !ok || urlStr == "" {
			continue
		}
		rewritten := urlrewrite.ApplyToURL(urlStr, rules)
		if rewritten != urlStr {
			client.Options["base_url"] = rewritten
		}
	}
}

func (w *workerImpl) CallStream(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
	method, ok := baml_rest.Methods[methodName]
	if !ok {
		return nil, fmt.Errorf("method %q not found", methodName)
	}

	// Parse input — the typed input struct ignores unknown fields like __baml_options__
	input := method.MakeInput()
	if err := json.Unmarshal(inputJSON, input); err != nil {
		return nil, fmt.Errorf("failed to unmarshal input: %w", err)
	}

	// Parse options separately — only extracts __baml_options__ field.
	// This is a second pass over the same JSON. A single-pass approach would
	// require a combined struct, but the input type is generated per-method
	// and not known at compile time. The cost is minor for typical payloads.
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

	return bridgeStreamResults(ctx, resultChan, w.logger), nil
}

// bridgeStreamResults converts adapter stream results into plugin stream results
// while respecting cancellation both before reading upstream and before sending
// downstream.
func bridgeStreamResults(ctx context.Context, resultChan <-chan bamlutils.StreamResult, logger bamlutils.Logger) <-chan *workerplugin.StreamResult {
	out := make(chan *workerplugin.StreamResult)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				go drainStreamResults(resultChan, logger)
				return
			default:
			}

			var (
				result bamlutils.StreamResult
				ok     bool
			)

			select {
			case <-ctx.Done():
				go drainStreamResults(resultChan, logger)
				return
			case result, ok = <-resultChan:
				if !ok {
					return
				}
			}

			pluginResult := workerplugin.GetStreamResult()
			pluginResult.Reset = result.Reset()

			switch result.Kind() {
			case bamlutils.StreamResultKindError:
				pluginResult.Kind = workerplugin.StreamResultKindError
				pluginResult.Error = result.Error()
			case bamlutils.StreamResultKindStream:
				pluginResult.Kind = workerplugin.StreamResultKindStream
				pluginResult.Raw = result.Raw()
				// Reset-only stream results intentionally carry no payload. If we
				// marshal nil here, it becomes JSON `null`, which downstream would
				// publish as a bogus partial frame in addition to the reset event.
				if !(result.Reset() && result.Stream() == nil) {
					data, err := json.Marshal(result.Stream())
					if err != nil {
						pluginResult.Kind = workerplugin.StreamResultKindError
						pluginResult.Error = fmt.Errorf("failed to marshal stream result: %w", err)
					} else {
						pluginResult.Data = data
					}
				} else {
					pluginResult.Raw = ""
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
			case bamlutils.StreamResultKindMetadata:
				md := result.Metadata()
				if md == nil {
					// An orchestrator bug — drop the event rather than crashing the stream.
					pluginResult.Kind = workerplugin.StreamResultKindError
					pluginResult.Error = fmt.Errorf("metadata result without payload")
					break
				}
				data, err := json.Marshal(md)
				if err != nil {
					pluginResult.Kind = workerplugin.StreamResultKindError
					pluginResult.Error = fmt.Errorf("failed to marshal metadata result: %w", err)
				} else {
					pluginResult.Kind = workerplugin.StreamResultKindMetadata
					pluginResult.Data = data
				}
			}

			// Release the adapter's output struct back to its pool
			result.Release()

			select {
			case out <- pluginResult:
			case <-ctx.Done():
				// Release the plugin result we couldn't send
				workerplugin.ReleaseStreamResult(pluginResult)
				go drainStreamResults(resultChan, logger)
				return
			}
		}
	}()

	return out
}

// drainStreamResults consumes and releases remaining results from the BAML
// adapter's stream channel after the bridge goroutine exits due to context
// cancellation. Each result holds native (Rust) memory via Release(); failing
// to drain leaves those resources leaked and can block the producer goroutine
// on an unbuffered send.
//
// The function drains until the channel is closed (i.e. the producer finishes).
// There is intentionally no hard timeout: a timeout would cause the drain
// goroutine to exit while the producer is still alive, stranding unreleased
// native results and blocking the producer on its next send.
//
// However, if the producer has not closed the channel after drainLeakThreshold
// (default 30s), a warning is logged so operators can investigate. The drain
// goroutine is also tracked in activeDrainGoroutines for monitoring.
func drainStreamResults(resultChan <-chan bamlutils.StreamResult, logger bamlutils.Logger) {
	activeDrainGoroutines.Add(1)
	defer activeDrainGoroutines.Add(-1)

	// Start a background timer that fires a warning if the drain takes
	// too long. The done channel signals the timer goroutine to exit
	// when the drain completes before the threshold.
	threshold := getDrainLeakThreshold()
	done := make(chan struct{})
	timer := time.NewTimer(threshold)
	go func() {
		select {
		case <-timer.C:
			active := activeDrainGoroutines.Load()
			if logger != nil {
				logger.Warn("drain goroutine still waiting for producer to close result channel",
					"waited", threshold.String(),
					"active_drain_goroutines", active,
				)
			}
		case <-done:
		}
	}()

	for result := range resultChan {
		result.Release()
	}

	timer.Stop()
	close(done)
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
