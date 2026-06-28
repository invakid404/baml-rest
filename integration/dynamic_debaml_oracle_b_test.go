//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/internal/schema/outputformat"
)

// De-BAML production wiring, GitHub #536 — Oracle B.
//
// For schemas that DO carry metadata BAML's dynamic TypeBuilder drops
// (class descriptions, field descriptions, field aliases, enum-level
// aliases), flag-ON intentionally differs from flag-OFF: native ON
// renders BAML's static/correct output-format semantics, which include
// that metadata.
//
// Class-level ALIASES are deliberately NOT in this divergence set: BAML's
// static ctx.output_format does not render class aliases (class bodies,
// refs, and hoisted-definition headers all use canonical names; only
// fields and enums use rendered aliases). The native renderer's use of
// canonical class names is therefore correct, and this test asserts a
// class alias is absent from BOTH ON and OFF rather than treating it as a
// divergence.
//
// The oracle for ON is BAML's STATIC render. We use the native renderer
// (internal/schema/outputformat) as that oracle: it is pinned byte-for-
// byte against BAML's own static render-output goldens by the
// outputformat package's golden tests, and it builds the same synthetic
// target class name (Baml_Rest_DynamicOutput) the production dynamic path
// uses, so the byte comparison holds even when definitions are hoisted —
// without needing a live static-BAML function in the integration harness.
// (The field-ALIAS divergence is independently anchored by the
// class_field_alias / class_with_field_alias golden in the outputformat
// corpus — a BAML-static-derived `want`, not the Go renderer's output —
// so this oracle is not circular for it.) The complementary differential — that ON drops nothing OFF keeps
// and adds exactly the metadata OFF lacks — is pinned by the explicit
// present/absent token assertions below.

// oracleBSchema carries the #536 divergence-set fields: a field-level
// description + alias, a class-level description, and an enum-level alias
// (with a value description so the enum hoists and its rendered header —
// alias under ON, canonical name under OFF — is observable in the bare
// ctx.output_format block). It also sets a class-level alias to assert it
// is NOT prompt-visible (canonical-name behaviour) under either flag.
func oracleBSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: dProps(
			dProp("title", &bamlutils.DynamicProperty{
				Type:        "string",
				Description: "The headline.",
				Alias:       "headline",
			}),
			dProp("addr", &bamlutils.DynamicProperty{Ref: "Address"}),
			dProp("status", &bamlutils.DynamicProperty{Ref: "Status"}),
		),
		Classes: dClasses(
			dClass("Address", &bamlutils.DynamicClass{
				Description: "A postal address.",
				// Class alias is intentionally set but NOT a divergence:
				// asserted absent from both ON and OFF below.
				Alias: "PostalAddress",
				Properties: dProps(
					dProp("street", &bamlutils.DynamicProperty{
						Type:        "string",
						Description: "Street line.",
						Alias:       "line1",
					}),
					dProp("city", &bamlutils.DynamicProperty{Type: "string"}),
				),
			}),
		),
		Enums: dEnums(
			dEnum("Status", &bamlutils.DynamicEnum{
				Alias: "State",
				Values: []*bamlutils.DynamicEnumValue{
					// Value description forces the enum to hoist (and is
					// itself propagated by BAML-dynamic, so it is NOT a
					// divergence — only the enum-level alias is).
					{Name: "ACTIVE", Description: "is active"},
					{Name: "INACTIVE"},
				},
			}),
		),
	}
}

const oracleBMock = `{"title":"t","addr":{"street":"s","city":"c"},"status":"ACTIVE"}`

// outputFormatFirstMessage returns the two-message request whose first
// message is ONLY the output_format part, so extractFirstMessageText
// recovers exactly the rendered block.
func outputFormatFirstMessage() []dynclient.Message {
	return []dynclient.Message{
		{Role: "system", PartsContent: []dynclient.ContentPart{{Type: "output_format"}}},
		{Role: "user", TextContent: strPtr("Produce the structured output.")},
	}
}

// TestDeBAMLOracleB_MetadataDivergence proves the deliberate "BAML done
// right" behaviour change behind the default-OFF master flag:
//
//  1. flag-ON's rendered block matches the native renderer byte-for-byte
//     (the BAML-static oracle);
//  2. flag-ON differs from flag-OFF (BAML-dynamic drops the metadata);
//  3. flag-ON includes, and flag-OFF omits, each divergent metadatum.
func TestDeBAMLOracleB_MetadataDivergence(t *testing.T) {
	debamlGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	offClient, onClient := newDeBAMLClients(t)

	scenarioID := "test-debaml-b-mixed"
	opts := setupNonStreamingScenario(t, scenarioID, oracleBMock)
	reg := dynRegistry(opts.ClientRegistry)

	offBody := captureCallBody(t, ctx, offClient, scenarioID, dynclient.Request{
		Messages: outputFormatFirstMessage(), ClientRegistry: reg, OutputSchema: oracleBSchema(),
	})
	onBody := captureCallBody(t, ctx, onClient, scenarioID, dynclient.Request{
		Messages: outputFormatFirstMessage(), ClientRegistry: reg, OutputSchema: oracleBSchema(),
	})

	offBlock, err := extractFirstMessageText(offBody)
	if err != nil {
		t.Fatalf("extractFirstMessageText(off): %v\n%s", err, offBody)
	}
	onBlock, err := extractFirstMessageText(onBody)
	if err != nil {
		t.Fatalf("extractFirstMessageText(on): %v\n%s", err, onBody)
	}

	// (1) ON matches the native renderer (the BAML-static oracle).
	bundle, err := schema.FromDynamicOutputSchema(oracleBSchema(), schema.BuildOptions{})
	if err != nil {
		t.Fatalf("FromDynamicOutputSchema: %v", err)
	}
	wantNative, err := outputformat.Render(bundle, outputformat.Options{})
	if err != nil {
		t.Fatalf("outputformat.Render: %v", err)
	}
	if onBlock != wantNative {
		t.Errorf("flag-ON block did not match the native (BAML-static) renderer\n--- want (native) ---\n%q\n--- got (ON) ---\n%q", wantNative, onBlock)
	}

	// (2) ON intentionally differs from OFF.
	if onBlock == offBlock {
		t.Fatalf("flag-ON block must differ from flag-OFF for a metadata-bearing schema, but they were identical:\n%q", onBlock)
	}

	// (3) Each divergent metadatum is present under ON and absent under OFF.
	for _, tok := range []struct{ what, token string }{
		{"field description", "The headline."},
		{"field alias", "headline:"},
		{"class description", "A postal address."},
		{"field-of-class description", "Street line."},
		{"field-of-class alias", "line1:"},
		{"enum alias (hoisted header)", "State\n----"},
	} {
		if !strings.Contains(onBlock, tok.token) {
			t.Errorf("flag-ON block missing %s %q\n--- ON ---\n%s", tok.what, tok.token, onBlock)
		}
		if strings.Contains(offBlock, tok.token) {
			t.Errorf("flag-OFF block unexpectedly contains %s %q (BAML-dynamic should drop it)\n--- OFF ---\n%s", tok.what, tok.token, offBlock)
		}
	}

	// And the canonical (un-aliased) forms survive under OFF, confirming
	// OFF really rendered the schema (just without the metadata).
	for _, tok := range []string{"title:", "Status\n----"} {
		if !strings.Contains(offBlock, tok) {
			t.Errorf("flag-OFF block missing expected canonical token %q\n--- OFF ---\n%s", tok, offBlock)
		}
	}

	// Class aliases are NOT a divergence: BAML static ctx.output_format
	// never renders them (canonical class names only). The schema sets a
	// class alias "PostalAddress"; it must be absent from BOTH blocks, and
	// the canonical class name path ("addr: {") present, proving the
	// native renderer's canonical-name behaviour is correct.
	for _, tok := range []string{"PostalAddress"} {
		if strings.Contains(onBlock, tok) {
			t.Errorf("flag-ON block unexpectedly contains class alias %q (class aliases are not prompt-visible)\n--- ON ---\n%s", tok, onBlock)
		}
		if strings.Contains(offBlock, tok) {
			t.Errorf("flag-OFF block unexpectedly contains class alias %q\n--- OFF ---\n%s", tok, offBlock)
		}
	}
}
