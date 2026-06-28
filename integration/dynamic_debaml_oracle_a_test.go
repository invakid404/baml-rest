//go:build integration

package integration

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/debaml"
)

// De-BAML production wiring, GitHub #536 — Oracle A.
//
// For schemas that carry NO metadata BAML's dynamic TypeBuilder drops
// (no class/field descriptions or aliases, no enum-level aliases), the
// native ctx.output_format renderer is byte-identical to BAML's, so the
// flag-ON provider request body must equal the flag-OFF body byte-for-
// byte. These tests drive the in-process dynclient over the BuildRequest
// route (the only route with the native seam, #537) and compare the full
// captured provider request bodies, not just the extracted block.
//
// ON and OFF share ONE scenario id so the request `model` field (the mock
// keys scenarios by model) is identical; the only thing that could differ
// is the output_format text, which for these fixtures must not.

// debamlGate restricts the de-BAML wiring tests to a BAML version whose
// BuildRequest API exposes the native seam. The dynclient clients below
// force WithUseBuildRequest(true) regardless of the suite's container
// mode, so this gates on the BAML version only.
func debamlGate(t *testing.T) {
	t.Helper()
	dynclientCallGate(t)
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
		t.Skip("de-BAML native output_format seam requires the BuildRequest route (BAML >= 0.219.0)")
	}
}

// newDeBAMLClients returns two in-process dynclients on the BuildRequest
// route: one with de-BAML off (BAML-as-today) and one with de-BAML on. The
// ON client is wired with the real native renderer (internal/debaml.Render)
// via WithDeBAMLRenderer — dynclient is a separate module and cannot import
// the renderer itself, so a root-module caller supplies it.
func newDeBAMLClients(t *testing.T) (off, on *dynclient.Client) {
	t.Helper()
	off = newDynclient(t, dynclient.WithUseBuildRequest(true))
	on = newDynclient(t,
		dynclient.WithUseBuildRequest(true),
		dynclient.WithDeBAML(true),
		dynclient.WithDeBAMLRenderer(debaml.Render),
	)
	return off, on
}

// captureCallBody runs a non-streaming dynamic call and returns the raw
// provider request body the mock captured for scenarioID. The call may
// error during response parsing; the request is captured before the
// response is parsed, so the body is available regardless.
func captureCallBody(t *testing.T, ctx context.Context, client *dynclient.Client, scenarioID string, req dynclient.Request) []byte {
	t.Helper()
	if _, err := client.DynamicCall(ctx, req); err != nil {
		t.Logf("DynamicCall (request still captured): %v", err)
	}
	body, err := MockClient.GetLastRequest(ctx, scenarioID)
	if err != nil {
		t.Fatalf("GetLastRequest(%s): %v", scenarioID, err)
	}
	return body
}

// captureStreamBody runs a streaming dynamic call (BuildRequest streaming
// closure) and returns the captured provider request body.
func captureStreamBody(t *testing.T, ctx context.Context, client *dynclient.Client, scenarioID string, req dynclient.Request) []byte {
	t.Helper()
	stream, err := client.DynamicStream(ctx, req)
	if err != nil {
		t.Fatalf("DynamicStream: %v", err)
	}
	defer stream.Close()
	drainLibStream(t, stream)
	body, err := MockClient.GetLastRequest(ctx, scenarioID)
	if err != nil {
		t.Fatalf("GetLastRequest(%s): %v", scenarioID, err)
	}
	return body
}

// oracleASchema is a metadata-free schema: primitives, a referenced class
// (no class/field descriptions or aliases), and an inline enum (<=6
// values, no descriptions or alias). Nothing here is lossy under BAML's
// dynamic TypeBuilder, so native render == BAML render.
func oracleASchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: dProps(
			dProp("answer", &bamlutils.DynamicProperty{Type: "string"}),
			dProp("count", &bamlutils.DynamicProperty{Type: "int"}),
			dProp("address", &bamlutils.DynamicProperty{Ref: "Address"}),
			dProp("status", &bamlutils.DynamicProperty{Ref: "Status"}),
		),
		Classes: dClasses(
			dClass("Address", &bamlutils.DynamicClass{
				Properties: dProps(
					dProp("street", &bamlutils.DynamicProperty{Type: "string"}),
					dProp("city", &bamlutils.DynamicProperty{Type: "string"}),
				),
			}),
		),
		Enums: dEnums(
			dEnum("Status", &bamlutils.DynamicEnum{
				Values: []*bamlutils.DynamicEnumValue{{Name: "ACTIVE"}, {Name: "INACTIVE"}},
			}),
		),
	}
}

const oracleAMock = `{"answer":"hi","count":1,"address":{"street":"s","city":"c"},"status":"ACTIVE"}`

// oracleAMessages enumerates the output-format insertion shapes that must
// stay byte-identical between flag-OFF and flag-ON.
func oracleAMessageCases() []struct {
	name string
	msgs func() []dynclient.Message
} {
	ofPart := dynclient.ContentPart{Type: "output_format"}
	textPart := func(s string) dynclient.ContentPart { return dynclient.ContentPart{Type: "text", Text: strPtr(s)} }

	return []struct {
		name string
		msgs func() []dynclient.Message
	}{
		{
			name: "output_format_part",
			msgs: func() []dynclient.Message {
				return []dynclient.Message{
					{Role: "system", PartsContent: []dynclient.ContentPart{ofPart}},
					{Role: "user", TextContent: strPtr("Go.")},
				}
			},
		},
		{
			name: "string_placeholder",
			msgs: func() []dynclient.Message {
				return []dynclient.Message{
					{Role: "system", TextContent: strPtr("Reply with:\n{output_format}\nThanks.")},
					{Role: "user", TextContent: strPtr("Go.")},
				}
			},
		},
		{
			name: "between_text_parts",
			msgs: func() []dynclient.Message {
				return []dynclient.Message{
					{Role: "system", PartsContent: []dynclient.ContentPart{
						textPart("Intro before.\n"), ofPart, textPart("\nOutro after."),
					}},
					{Role: "user", TextContent: strPtr("Go.")},
				}
			},
		},
		{
			name: "multi_message",
			msgs: func() []dynclient.Message {
				return []dynclient.Message{
					{Role: "system", PartsContent: []dynclient.ContentPart{ofPart}},
					{Role: "user", TextContent: strPtr("First question.")},
					{Role: "assistant", TextContent: strPtr("First answer.")},
					{Role: "user", TextContent: strPtr("Second question with {output_format} inline.")},
				}
			},
		},
	}
}

// TestDeBAMLOracleA_NonStreaming asserts flag-ON == flag-OFF byte-for-byte
// on the non-streaming BuildRequest closure for every insertion shape.
func TestDeBAMLOracleA_NonStreaming(t *testing.T) {
	debamlGate(t)
	offClient, onClient := newDeBAMLClients(t)

	for _, tc := range oracleAMessageCases() {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			scenarioID := "test-debaml-a-call-" + tc.name
			opts := setupNonStreamingScenario(t, scenarioID, oracleAMock)
			reg := dynRegistry(opts.ClientRegistry)

			offBody := captureCallBody(t, ctx, offClient, scenarioID, dynclient.Request{
				Messages: tc.msgs(), ClientRegistry: reg, OutputSchema: oracleASchema(),
			})
			onBody := captureCallBody(t, ctx, onClient, scenarioID, dynclient.Request{
				Messages: tc.msgs(), ClientRegistry: reg, OutputSchema: oracleASchema(),
			})

			if !bytes.Equal(offBody, onBody) {
				t.Errorf("flag-ON provider body diverged from flag-OFF (no-metadata schema must be byte-identical)\n--- OFF ---\n%s\n--- ON ---\n%s", offBody, onBody)
			}
		})
	}
}

// TestDeBAMLOracleA_Streaming covers the streaming BuildRequest closure
// (and, structurally, the Bedrock closure: the substitution runs once at
// the top of bamlRestDynamicBuildRequest, before every closure including
// buildBedrockStreamRequestFn).
func TestDeBAMLOracleA_Streaming(t *testing.T) {
	debamlGate(t)
	offClient, onClient := newDeBAMLClients(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	scenarioID := "test-debaml-a-stream"
	opts := setupScenario(t, scenarioID, oracleAMock)
	reg := dynRegistry(opts.ClientRegistry)

	msgs := func() []dynclient.Message {
		return []dynclient.Message{
			{Role: "system", PartsContent: []dynclient.ContentPart{{Type: "output_format"}}},
			{Role: "user", TextContent: strPtr("Go.")},
		}
	}

	offBody := captureStreamBody(t, ctx, offClient, scenarioID, dynclient.Request{
		Messages: msgs(), ClientRegistry: reg, OutputSchema: oracleASchema(),
	})
	onBody := captureStreamBody(t, ctx, onClient, scenarioID, dynclient.Request{
		Messages: msgs(), ClientRegistry: reg, OutputSchema: oracleASchema(),
	})

	if !bytes.Equal(offBody, onBody) {
		t.Errorf("streaming flag-ON provider body diverged from flag-OFF\n--- OFF ---\n%s\n--- ON ---\n%s", offBody, onBody)
	}
}

// TestDeBAMLOracleA_BuildRequestFlagSeparation pins the accepted route
// coupling (#537): with BAML_REST_USE_BUILD_REQUEST off, the request stays
// on the legacy BAML path and the de-BAML flag is a no-op, so flag-ON ==
// flag-OFF even for a metadata-bearing schema (the seam is never reached).
// This also proves the two flags are independent: WithDeBAML does not
// implicitly enable BuildRequest.
func TestDeBAMLOracleA_BuildRequestFlagSeparation(t *testing.T) {
	debamlGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	scenarioID := "test-debaml-a-flagsep"
	opts := setupNonStreamingScenario(t, scenarioID, oracleAMock)
	reg := dynRegistry(opts.ClientRegistry)

	// Both clients leave BuildRequest OFF (the dynclient default); one
	// still asks for de-BAML. The legacy route has no native seam, so the
	// bodies must match even though the schema carries divergent metadata.
	// The ON client has de-BAML fully enabled AND the renderer wired, so
	// the no-op is attributable to the route (BuildRequest off), not a
	// missing renderer.
	legacyOff := newDynclient(t)
	legacyOnButNoBuildRequest := newDynclient(t, dynclient.WithDeBAML(true), dynclient.WithDeBAMLRenderer(debaml.Render))

	offBody := captureCallBody(t, ctx, legacyOff, scenarioID, dynclient.Request{
		Messages:       []dynclient.Message{{Role: "system", PartsContent: []dynclient.ContentPart{{Type: "output_format"}}}, {Role: "user", TextContent: strPtr("Go.")}},
		ClientRegistry: reg, OutputSchema: oracleBSchema(),
	})
	onBody := captureCallBody(t, ctx, legacyOnButNoBuildRequest, scenarioID, dynclient.Request{
		Messages:       []dynclient.Message{{Role: "system", PartsContent: []dynclient.ContentPart{{Type: "output_format"}}}, {Role: "user", TextContent: strPtr("Go.")}},
		ClientRegistry: reg, OutputSchema: oracleBSchema(),
	})

	if !bytes.Equal(offBody, onBody) {
		t.Errorf("de-BAML must be a no-op off the BuildRequest route (accepted #537 coupling), but bodies differed\n--- OFF ---\n%s\n--- ON ---\n%s", offBody, onBody)
	}
}
