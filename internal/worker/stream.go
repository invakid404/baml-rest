package worker

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/apierror"
	"github.com/invakid404/baml-rest/workerplugin"
)

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

// bridgeStreamResults converts adapter stream results into plugin stream results
// while respecting cancellation both before reading upstream and before sending
// downstream.
func bridgeStreamResults(ctx context.Context, resultChan <-chan bamlutils.StreamResult, logger bamlutils.Logger) <-chan *workerplugin.StreamResult {
	out := make(chan *workerplugin.StreamResult)
	go func() {
		// Defer order is load-bearing: close(out) is registered first
		// so it fires LAST and the recover defer (registered after,
		// fires first) can publish a terminal error frame onto an
		// open channel before close releases it. recoverBridgePanic
		// is a no-op in subprocess builds.
		defer close(out)
		defer recoverBridgePanic(ctx, out, logger)
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
				// Classify typed BuildRequest / transport surfaces into a
				// worker-facing apierror.Code (parse_error / provider_error)
				// so the host's classifyWorkerError forwards it verbatim
				// instead of defaulting to worker_error. classifyBAMLError
				// returns ("", nil) for unrecognized errors, leaving
				// pluginResult.ErrorCode at its zero value for host-side
				// fallback.
				//
				// mergeRawDetail attaches the accumulator's text (per #256)
				// to details regardless of classification arm: parse_error,
				// provider_error, and unclassified errors all carry
				// details.raw when result.Raw() is non-empty. Unclassified
				// errors keep their empty worker code but gain details.raw
				// via the host's worker_error fallback path. Empty raw
				// leaves details untouched (omitempty contract).
				code, details := classifyBAMLError(result.Error())
				details = mergeRawDetail(details, result.Raw())
				if code != "" {
					pluginResult.ErrorCode = code
				}
				if details != nil {
					pluginResult.ErrorDetails = details
				}
			case bamlutils.StreamResultKindStream:
				pluginResult.Kind = workerplugin.StreamResultKindStream
				// Reset-only stream results intentionally carry no payload. If we
				// marshal nil here, it becomes JSON `null`, which downstream would
				// publish as a bogus partial frame in addition to the reset event.
				// Raw and reasoning stay at their zero values for reset-only
				// frames — the orchestrator clears the accumulators downstream.
				if !(result.Reset() && result.Stream() == nil) {
					data, err := json.Marshal(result.Stream())
					if err != nil {
						// Marshal failed: reclassify as an error frame. Raw/
						// reasoning must stay unset — leaking stale values
						// from the doomed stream frame onto an error frame
						// would publish accumulated bytes alongside an error
						// the client cannot pair them with. Assigning raw/
						// reasoning only on the success branch below
						// guarantees this invariant.
						pluginResult.Kind = workerplugin.StreamResultKindError
						pluginResult.Error = fmt.Errorf("failed to marshal stream result: %w", err)
						pluginResult.ErrorCode = string(apierror.CodeInternalError)
					} else {
						pluginResult.Data = data
						pluginResult.Raw = result.Raw()
						pluginResult.Reasoning = result.Reasoning()
					}
				}
			case bamlutils.StreamResultKindFinal:
				data, err := json.Marshal(result.Final())
				if err != nil {
					pluginResult.Kind = workerplugin.StreamResultKindError
					pluginResult.Error = fmt.Errorf("failed to marshal final result: %w", err)
					pluginResult.ErrorCode = string(apierror.CodeInternalError)
				} else {
					pluginResult.Kind = workerplugin.StreamResultKindFinal
					pluginResult.Data = data
					pluginResult.Raw = result.Raw()
					pluginResult.Reasoning = result.Reasoning()
				}
			case bamlutils.StreamResultKindHeartbeat:
				pluginResult.Kind = workerplugin.StreamResultKindHeartbeat
			case bamlutils.StreamResultKindMetadata:
				md := result.Metadata()
				if md == nil {
					// An orchestrator bug — drop the event rather than crashing the stream.
					pluginResult.Kind = workerplugin.StreamResultKindError
					pluginResult.Error = fmt.Errorf("metadata result without payload")
					pluginResult.ErrorCode = string(apierror.CodeInternalError)
					break
				}
				data, err := json.Marshal(md)
				if err != nil {
					pluginResult.Kind = workerplugin.StreamResultKindError
					pluginResult.Error = fmt.Errorf("failed to marshal metadata result: %w", err)
					pluginResult.ErrorCode = string(apierror.CodeInternalError)
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
