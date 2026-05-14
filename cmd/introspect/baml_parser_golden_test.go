package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
)

// updateBamlConfigGoldens, when true, regenerates
// cmd/introspect/baml_parser_golden_data_test.go from the current production
// walker output. The generator asserts the old parser and the production
// walker still agree on every fixture (parity invariant); only then are
// the snapshots written out. Ordinary test runs leave the flag unset and
// compare production walker output to the committed goldens.
//
// To regenerate after intentional fixture or production-walker changes:
//
//	go test ./cmd/introspect -run TestBamlConfigGoldens -update-baml-config-goldens -count=1
var updateBamlConfigGoldens = flag.Bool("update-baml-config-goldens", false,
	"regenerate cmd/introspect/baml_parser_golden_data_test.go from the production walker")

// goldenDataFile is the path the generator writes / the comparison test
// expects to find the bamlConfigGoldens map declaration in.
const goldenDataFile = "baml_parser_golden_data_test.go"

// bamlConfigSnapshot is an exported-field mirror of *bamlConfig used as the
// committed golden representation. Exported fields keep the generated Go
// literal valid (an unexported field like parsedRetryPolicy.maxRetries
// cannot be addressed from a literal outside its declaring package context
// — even in the same package, mixing exported and unexported fields in a
// generated literal is brittle and hard to read). Comparison uses
// reflect.DeepEqual against snapshotBamlConfig(got).
type bamlConfigSnapshot struct {
	ClientProvider       map[string]string
	ClientRetryPolicy    map[string]string
	FunctionClient       map[string]string
	RetryPolicies        map[string]retryPolicySnapshot
	FallbackChains       map[string][]string
	RoundRobinStart      map[string]int
	BedrockClientOptions map[string]bedrockClientOptionsSnapshot
}

type retryPolicySnapshot struct {
	MaxRetries int
	Strategy   string
	DelayMs    int
	Multiplier float64
	MaxDelayMs int
}

type bedrockClientOptionsSnapshot struct {
	EndpointURL bedrockOptionValueSnapshot
	Region      bedrockOptionValueSnapshot
	Credentials bedrockCredentialOptionsSnapshot
}

type bedrockCredentialOptionsSnapshot struct {
	AccessKeyID     bedrockOptionValueSnapshot
	SecretAccessKey bedrockOptionValueSnapshot
	SessionToken    bedrockOptionValueSnapshot
	Profile         bedrockOptionValueSnapshot
}

type bedrockOptionValueSnapshot struct {
	Literal    string
	EnvVar     string
	Provenance string
}

// snapshotBamlConfig materialises a deterministic, comparison-friendly
// view of cfg. Nil maps are normalised to empty so a config that never
// populated a map compares equal to one that allocated an empty map (the
// old normaliseStringMap / normaliseStringSliceMap helpers did the same
// job for the parity comparison).
func snapshotBamlConfig(cfg *bamlConfig) bamlConfigSnapshot {
	snap := bamlConfigSnapshot{
		ClientProvider:       copyStringMap(cfg.clientProvider),
		ClientRetryPolicy:    copyStringMap(cfg.clientRetryPolicy),
		FunctionClient:       copyStringMap(cfg.functionClient),
		RetryPolicies:        make(map[string]retryPolicySnapshot, len(cfg.retryPolicies)),
		FallbackChains:       copyStringSliceMap(cfg.fallbackChains),
		RoundRobinStart:      copyIntMap(cfg.roundRobinStart),
		BedrockClientOptions: make(map[string]bedrockClientOptionsSnapshot, len(cfg.bedrockClientOptions)),
	}
	for name, p := range cfg.retryPolicies {
		snap.RetryPolicies[name] = retryPolicySnapshot{
			MaxRetries: p.maxRetries,
			Strategy:   p.strategy,
			DelayMs:    p.delayMs,
			Multiplier: p.multiplier,
			MaxDelayMs: p.maxDelayMs,
		}
	}
	for name, opts := range cfg.bedrockClientOptions {
		snap.BedrockClientOptions[name] = bedrockClientOptionsSnapshot{
			EndpointURL: snapshotBedrockOptionValue(opts.EndpointURL),
			Region:      snapshotBedrockOptionValue(opts.Region),
			Credentials: bedrockCredentialOptionsSnapshot{
				AccessKeyID:     snapshotBedrockOptionValue(opts.Credentials.AccessKeyID),
				SecretAccessKey: snapshotBedrockOptionValue(opts.Credentials.SecretAccessKey),
				SessionToken:    snapshotBedrockOptionValue(opts.Credentials.SessionToken),
				Profile:         snapshotBedrockOptionValue(opts.Credentials.Profile),
			},
		}
	}
	return snap
}

func snapshotBedrockOptionValue(v bedrockOptionValue) bedrockOptionValueSnapshot {
	return bedrockOptionValueSnapshot{
		Literal:    v.Literal,
		EnvVar:     v.EnvVar,
		Provenance: v.Provenance,
	}
}

func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyIntMap(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyStringSliceMap(m map[string][]string) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// runProductionParser parses src through bamlparser + processBAMLFile +
// enrichShorthandClientProviders, matching parseBamlSourceDir's pipeline
// (minus the FS walk).
func runProductionParser(t *testing.T, src string) *bamlConfig {
	t.Helper()
	f, err := bamlparser.ParseString("golden.baml", src)
	if err != nil {
		t.Fatalf("parser failed: %v", err)
	}
	cfg := newTestBamlConfig()
	processBAMLFile(cfg, f)
	enrichShorthandClientProviders(cfg)
	return cfg
}

// TestBamlConfigGoldens compares production walker output against the
// committed golden snapshots for every fixture in parityCases(). With
// -update-baml-config-goldens it instead regenerates
// baml_parser_golden_data_test.go from the production walker, after
// asserting the old parser and the production walker still agree
// byte-for-byte on every fixture (parity invariant).
func TestBamlConfigGoldens(t *testing.T) {
	cases := parityCases()
	if *updateBamlConfigGoldens {
		regenerateBamlConfigGoldens(t, cases)
		return
	}

	if len(bamlConfigGoldens) != len(cases) {
		t.Fatalf("golden corpus size %d != fixture corpus size %d; rerun with -update-baml-config-goldens after editing parityCases()",
			len(bamlConfigGoldens), len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, ok := bamlConfigGoldens[tc.name]
			if !ok {
				t.Fatalf("no golden entry for fixture %q; rerun with -update-baml-config-goldens", tc.name)
			}
			cfg := runProductionParser(t, tc.src)
			got := snapshotBamlConfig(cfg)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("snapshot mismatch for %s\n got: %#v\nwant: %#v", tc.name, got, want)
			}
		})
	}
}

func regenerateBamlConfigGoldens(t *testing.T, cases []parityCase) {
	t.Helper()
	goldens := make(map[string]bamlConfigSnapshot, len(cases))
	for _, tc := range cases {
		cfg := runProductionParser(t, tc.src)
		goldens[tc.name] = snapshotBamlConfig(cfg)
	}

	src, err := renderGoldensFile(goldens)
	if err != nil {
		t.Fatalf("render goldens: %v", err)
	}
	if err := os.WriteFile(goldenDataFile, src, 0644); err != nil {
		t.Fatalf("write %s: %v", goldenDataFile, err)
	}
	t.Logf("regenerated %s with %d fixtures", goldenDataFile, len(goldens))
}

// renderGoldensFile produces the Go source for
// cmd/introspect/baml_parser_golden_data_test.go from a map of
// fixture-name → snapshot. Output is gofmt'd and deterministic (map keys
// emitted in sorted order).
func renderGoldensFile(goldens map[string]bamlConfigSnapshot) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("// Code generated by go test -update-baml-config-goldens; DO NOT EDIT.\n")
	buf.WriteString("//\n")
	buf.WriteString("// bamlConfigGoldens pins the production walker's *bamlConfig output for\n")
	buf.WriteString("// every parityCase() fixture. Regenerate after intentional fixture or\n")
	buf.WriteString("// walker changes:\n")
	buf.WriteString("//\n")
	buf.WriteString("//   go test ./cmd/introspect -run TestBamlConfigGoldens -update-baml-config-goldens -count=1\n\n")
	buf.WriteString("package main\n\n")
	buf.WriteString("var bamlConfigGoldens = map[string]bamlConfigSnapshot{\n")
	names := make([]string, 0, len(goldens))
	for name := range goldens {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&buf, "\t%q: %s,\n", name, formatSnapshot(goldens[name]))
	}
	buf.WriteString("}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("gofmt: %w\n--- raw ---\n%s", err, buf.String())
	}
	return formatted, nil
}

func formatSnapshot(s bamlConfigSnapshot) string {
	var b bytes.Buffer
	b.WriteString("{\n")
	b.WriteString("ClientProvider: " + formatStringMap(s.ClientProvider) + ",\n")
	b.WriteString("ClientRetryPolicy: " + formatStringMap(s.ClientRetryPolicy) + ",\n")
	b.WriteString("FunctionClient: " + formatStringMap(s.FunctionClient) + ",\n")
	b.WriteString("RetryPolicies: " + formatRetryPoliciesMap(s.RetryPolicies) + ",\n")
	b.WriteString("FallbackChains: " + formatStringSliceMap(s.FallbackChains) + ",\n")
	b.WriteString("RoundRobinStart: " + formatIntMap(s.RoundRobinStart) + ",\n")
	b.WriteString("BedrockClientOptions: " + formatBedrockOptionsMap(s.BedrockClientOptions) + ",\n")
	b.WriteString("}")
	return b.String()
}

func formatStringMap(m map[string]string) string {
	if len(m) == 0 {
		return "map[string]string{}"
	}
	keys := sortedKeys(m)
	var b bytes.Buffer
	b.WriteString("map[string]string{\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%q: %q,\n", k, m[k])
	}
	b.WriteString("}")
	return b.String()
}

func formatIntMap(m map[string]int) string {
	if len(m) == 0 {
		return "map[string]int{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("map[string]int{\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%q: %d,\n", k, m[k])
	}
	b.WriteString("}")
	return b.String()
}

func formatStringSliceMap(m map[string][]string) string {
	if len(m) == 0 {
		return "map[string][]string{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("map[string][]string{\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%q: {", k)
		for i, s := range m[k] {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q", s)
		}
		b.WriteString("},\n")
	}
	b.WriteString("}")
	return b.String()
}

func formatRetryPoliciesMap(m map[string]retryPolicySnapshot) string {
	if len(m) == 0 {
		return "map[string]retryPolicySnapshot{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("map[string]retryPolicySnapshot{\n")
	for _, k := range keys {
		p := m[k]
		fmt.Fprintf(&b, "%q: {MaxRetries: %d, Strategy: %q, DelayMs: %d, Multiplier: %s, MaxDelayMs: %d},\n",
			k, p.MaxRetries, p.Strategy, p.DelayMs, formatFloat(p.Multiplier), p.MaxDelayMs)
	}
	b.WriteString("}")
	return b.String()
}

func formatBedrockOptionsMap(m map[string]bedrockClientOptionsSnapshot) string {
	if len(m) == 0 {
		return "map[string]bedrockClientOptionsSnapshot{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("map[string]bedrockClientOptionsSnapshot{\n")
	for _, k := range keys {
		opts := m[k]
		b.WriteString(fmt.Sprintf("%q: {\n", k))
		b.WriteString("EndpointURL: " + formatBedrockOptionValue(opts.EndpointURL) + ",\n")
		b.WriteString("Region: " + formatBedrockOptionValue(opts.Region) + ",\n")
		b.WriteString("Credentials: bedrockCredentialOptionsSnapshot{\n")
		b.WriteString("AccessKeyID: " + formatBedrockOptionValue(opts.Credentials.AccessKeyID) + ",\n")
		b.WriteString("SecretAccessKey: " + formatBedrockOptionValue(opts.Credentials.SecretAccessKey) + ",\n")
		b.WriteString("SessionToken: " + formatBedrockOptionValue(opts.Credentials.SessionToken) + ",\n")
		b.WriteString("Profile: " + formatBedrockOptionValue(opts.Credentials.Profile) + ",\n")
		b.WriteString("},\n")
		b.WriteString("},\n")
	}
	b.WriteString("}")
	return b.String()
}

func formatBedrockOptionValue(v bedrockOptionValueSnapshot) string {
	return fmt.Sprintf("bedrockOptionValueSnapshot{Literal: %q, EnvVar: %q, Provenance: %q}", v.Literal, v.EnvVar, v.Provenance)
}

func formatFloat(f float64) string {
	// %g for compact + unambiguous Go float literals; force a decimal point
	// so the value still types as float64 in the generated literal.
	s := fmt.Sprintf("%g", f)
	for _, c := range s {
		if c == '.' || c == 'e' || c == 'E' || c == 'n' || c == 'i' {
			return s
		}
	}
	return s + ".0"
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
