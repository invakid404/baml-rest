package main

import (
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
)

// bamlValidationErrorCases is the table of negative + regression-net
// cases for the per-client Bedrock-key validation surfaced by the
// production walker. The negative cases exercise upstream BAML's
// "region cannot be empty" error (aws_bedrock.rs:256-261) for declared
// `region ""` literals on aws-bedrock clients. The regression-net cases
// guard against accidentally tightening behaviour for other Bedrock
// keys (endpoint_url) or for non-Bedrock clients that happen to use the
// same key name. See cmd/introspect/main.go's bamlValueBedrockOption
// helper and the per-client provider-gating two-pass walk in
// processBAMLClientBlock.
var bamlValidationErrorCases = []struct {
	name    string
	src     string
	wantErr []string // substrings the accumulated error message must contain; empty/nil → expect no errors
}{
	{
		name: "BedrockRegionEmptyLiteralErrors",
		src: `
client<llm> EmptyRegion {
    provider aws-bedrock
    options {
        region ""
    }
}
`,
		wantErr: []string{`EmptyRegion`, `region`, `cannot be empty`},
	},
	{
		name: "BedrockRegionEmptyLiteralErrors_OptionsBeforeProvider",
		src: `
client<llm> EmptyRegion {
    options {
        region ""
    }
    provider aws-bedrock
}
`,
		wantErr: []string{`EmptyRegion`, `region`, `cannot be empty`},
	},
	{
		name: "RegionEmptyLiteralOnNonBedrockClient_NoError",
		src: `
client<llm> Other {
    provider openai
    options {
        region ""
    }
}
`,
		wantErr: nil,
	},
	{
		name: "BedrockEndpointURLEmptyLiteral_NoError",
		src: `
client<llm> EndpointOnly {
    provider aws-bedrock
    options {
        endpoint_url ""
        region "us-east-1"
    }
}
`,
		wantErr: nil,
	},
}

func TestBamlValidationErrors(t *testing.T) {
	for _, tc := range bamlValidationErrorCases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := bamlparser.ParseString("validation.baml", tc.src)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			cfg := newTestBamlConfig()
			processBAMLFile(cfg, f)

			if len(tc.wantErr) == 0 {
				if len(cfg.validationErrors) > 0 {
					t.Errorf("want no validation errors, got: %v", cfg.validationErrors)
				}
				return
			}

			if len(cfg.validationErrors) != 1 {
				t.Fatalf("want 1 validation error, got %d: %v",
					len(cfg.validationErrors), cfg.validationErrors)
			}
			got := cfg.validationErrors[0].Message()
			for _, want := range tc.wantErr {
				if !strings.Contains(got, want) {
					t.Errorf("validation error %q missing substring %q", got, want)
				}
			}
		})
	}
}
