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
// `region ""` literals on aws-bedrock clients, plus the parse-time
// rejection of declared-empty `access_key_id` / `secret_access_key`
// literals (#268: upstream BAML accepts them and signs with literally
// empty SigV4 creds, AWS then rejects at request time — failing at
// parse time is the friendlier operator signal). The regression-net
// cases guard against accidentally tightening behaviour for keys that
// remain empty-as-absent (endpoint_url, session_token, profile) or for
// non-Bedrock clients that happen to use the same key name. See
// cmd/introspect/main.go's bamlValueBedrockOption helper and the
// per-client provider-gating two-pass walk in processBAMLClientBlock.
var bamlValidationErrorCases = []struct {
	name         string
	src          string
	wantErr      []string // substrings the accumulated error message must contain; empty/nil → expect no errors
	wantErrCount int      // expected number of accumulated validationErrors; defaults to 1 when wantErr is non-empty
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
	{
		name: "BedrockAccessKeyIDEmptyLiteralErrors",
		src: `
client<llm> EmptyAKID {
    provider aws-bedrock
    options {
        access_key_id ""
        secret_access_key "STATIC_TEST_SECRET"
        region "us-east-1"
    }
}
`,
		wantErr: []string{`EmptyAKID`, `access_key_id`, `cannot be empty`},
	},
	{
		name: "BedrockAccessKeyIDEmptyLiteralErrors_OptionsBeforeProvider",
		src: `
client<llm> EmptyAKID {
    options {
        access_key_id ""
        secret_access_key "STATIC_TEST_SECRET"
        region "us-east-1"
    }
    provider aws-bedrock
}
`,
		wantErr: []string{`EmptyAKID`, `access_key_id`, `cannot be empty`},
	},
	{
		name: "BedrockSecretAccessKeyEmptyLiteralErrors",
		src: `
client<llm> EmptySecret {
    provider aws-bedrock
    options {
        access_key_id "STATIC_TEST_ACCESS_KEY"
        secret_access_key ""
        region "us-east-1"
    }
}
`,
		wantErr: []string{`EmptySecret`, `secret_access_key`, `cannot be empty`},
	},
	{
		name: "BedrockBothAccessKeyAndSecretEmptyErrors",
		src: `
client<llm> EmptyBoth {
    provider aws-bedrock
    options {
        access_key_id ""
        secret_access_key ""
        region "us-east-1"
    }
}
`,
		// Both empty fields must each surface as a distinct
		// accumulated validation error — pin that the walker
		// doesn't short-circuit after the first error within
		// one client block.
		wantErr:      []string{`EmptyBoth`, `access_key_id`, `secret_access_key`, `cannot be empty`},
		wantErrCount: 2,
	},
	{
		name: "BedrockSessionTokenEmptyLiteral_NoError",
		src: `
client<llm> EmptySessionToken {
    provider aws-bedrock
    options {
        access_key_id "STATIC_TEST_ACCESS_KEY"
        secret_access_key "STATIC_TEST_SECRET"
        session_token ""
        region "us-east-1"
    }
}
`,
		wantErr: nil,
	},
	{
		name: "BedrockProfileEmptyLiteral_NoError",
		src: `
client<llm> EmptyProfile {
    provider aws-bedrock
    options {
        profile ""
        region "us-east-1"
    }
}
`,
		wantErr: nil,
	},
	{
		name: "AccessKeyIDEmptyLiteralOnNonBedrockClient_NoError",
		src: `
client<llm> Other {
    provider openai
    options {
        access_key_id ""
        secret_access_key ""
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

			wantCount := tc.wantErrCount
			if wantCount == 0 {
				wantCount = 1
			}
			if len(cfg.validationErrors) != wantCount {
				t.Fatalf("want %d validation error(s), got %d: %v",
					wantCount, len(cfg.validationErrors), cfg.validationErrors)
			}
			// Combine all accumulated messages — substring assertions
			// span the multi-error case where each key produces its
			// own validation error within the same client block.
			parts := make([]string, len(cfg.validationErrors))
			for i, e := range cfg.validationErrors {
				parts[i] = e.Message()
			}
			got := strings.Join(parts, "\n")
			for _, want := range tc.wantErr {
				if !strings.Contains(got, want) {
					t.Errorf("validation error %q missing substring %q", got, want)
				}
			}
			// Pin the no-value-leak invariant: validation messages
			// must mention the field name + client name without
			// echoing the configured value. For the empty-literal
			// cases the configured value is `""`, so the literal
			// `""` substring is a sentinel for accidental
			// value-echoing. Same shape as #263's no-secret-leak
			// credential checks.
			for _, leaky := range []string{`""`, `value=`, `got=`} {
				if strings.Contains(got, leaky) {
					t.Errorf("validation error must not echo configured value; found %q in %q",
						leaky, got)
				}
			}
		})
	}
}
