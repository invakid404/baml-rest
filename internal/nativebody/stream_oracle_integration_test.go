//go:build integration

package nativebody

// De-BAML Phase 7B BAML-oracle byte-parity for the STREAMING canonical body: for
// every admitted fixture the native streaming body (BuildOpenAIChatStream) must
// equal BAML v0.223's actual StreamRequest body byte-for-byte. It is the
// streaming twin of TestNativeBodyDynamicOracleParity and reuses the SAME
// oracle harness (newOracleClient / oracleRegistry / bodyCases). Combined with
// the nanollm_integration admission oracle (nanollm Prepare(stream) ==
// BuildOpenAIChatStream, enforced by validatePreparedBody), this closes the
// chain nanollm-prepared == canonical == BAML for the streaming plan.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
)

// captureBAMLStreamBody captures BAML's actual StreamRequest body for a fixture.
// DynamicStream builds+issues the request; the canned non-SSE response only fails
// during draining, AFTER the request body is captured — so a healthy run yields a
// non-empty captured body regardless of the decode failure.
func captureBAMLStreamBody(t *testing.T, client *dynclient.Client, capture *captureRoundTripper, tc bodyCase) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	capture.reset()
	stream, err := client.DynamicStream(ctx, dynclient.Request{
		Messages:       toDynMessages(tc.messages),
		ClientRegistry: oracleRegistry(tc.clientOptions),
		OutputSchema:   tc.schema,
	})
	if err != nil {
		t.Fatalf("DynamicStream errored — stream request construction failed: %v", err)
	}
	if stream != nil {
		for {
			ev, nerr := stream.Next()
			if ev == nil || nerr != nil {
				break
			}
		}
		_ = stream.Close()
	}
	body := capture.lastBody()
	if len(body) == 0 {
		t.Fatalf("no BAML stream request captured — stream request construction failed before send")
	}
	return body
}

// TestNativeBodyStreamOracleParity proves BuildOpenAIChatStream == BAML's
// StreamRequest body byte-for-byte for every admitted fixture — the streaming
// exact-plan==BAML gate at the body level.
func TestNativeBodyStreamOracleParity(t *testing.T) {
	capture := &captureRoundTripper{}
	client := newOracleClient(t, capture)

	for _, tc := range bodyCases() {
		if !tc.admit {
			continue
		}
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			bamlStream := captureBAMLStreamBody(t, client, capture, tc)

			// Guard the control: BAML's stream body must carry both suffix fields,
			// else the harness regressed and a byte match would be meaningless.
			if !strings.Contains(string(bamlStream), `"stream":true`) ||
				!strings.Contains(string(bamlStream), `"stream_options":{"include_usage":true}`) {
				t.Fatalf("BAML stream body missing the stream suffix: %s", bamlStream)
			}

			rendered, err := nativeprompt.Render(tc.messages, tc.schema)
			if err != nil {
				t.Fatalf("nativeprompt.Render: %v", err)
			}
			intent, err := NormalizeDynamicClient(oracleRegistry(tc.clientOptions), oracleAlias, true /* stream */)
			if err != nil {
				t.Fatalf("NormalizeDynamicClient(stream): %v", err)
			}
			native, err := BuildOpenAIChatStream(rendered, intent)
			if err != nil {
				t.Fatalf("BuildOpenAIChatStream declined an admitted fixture: %v", err)
			}

			if !bytesEqual(native.Bytes(), bamlStream) {
				t.Errorf("native stream body diverged from BAML\n--- native ---\n%s\n--- BAML ---\n%s\n%s",
					native.Bytes(), bamlStream, jsonDiff(native.Bytes(), bamlStream))
			}
			// The nanollm alias must never leak into the JSON body.
			if strings.Contains(string(native.Bytes()), oracleAlias) {
				t.Errorf("nanollm alias leaked into stream JSON body: %s", native.Bytes())
			}
		})
	}
}
