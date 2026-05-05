package bamlutils

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

type StreamResult interface {
	Kind() StreamResultKind
	Stream() any
	Final() any
	Error() error
	// Raw returns the raw LLM response text at this streaming point
	Raw() string
	// Reset returns true if the client should discard accumulated state (retry occurred)
	Reset() bool
	// Metadata returns the routing/retry metadata payload. Non-nil only when
	// Kind()==StreamResultKindMetadata.
	Metadata() *Metadata
	// Release returns the StreamResult to a pool for reuse.
	// After calling Release, the StreamResult should not be accessed.
	Release()
}

type StreamResultKind int

const (
	StreamResultKindStream StreamResultKind = iota
	StreamResultKindFinal
	StreamResultKindError
	StreamResultKindHeartbeat
	StreamResultKindMetadata
)

// MetadataPhase distinguishes the two kinds of metadata events a request
// may emit. Planned events carry the routing decision (path, client, chain,
// retry policy). Outcome events carry the post-flight truth (winner client,
// actual retry count, upstream duration).
type MetadataPhase string

const (
	MetadataPhasePlanned MetadataPhase = "planned"
	MetadataPhaseOutcome MetadataPhase = "outcome"
)

// Metadata is the per-request routing/retry metadata payload emitted by the
// orchestrator(s) and consumed by the serve layer (as HTTP headers on unary
// responses and as in-stream events on streaming responses).
//
// Pointer fields distinguish "unknown" (nil) from "zero" (pointer to 0) so
// absence can be represented faithfully. Some outcome fields are populated
// only on one path (e.g. RetryCount on BuildRequest, BamlCallCount on
// legacy) — see per-field doc strings for the contract.
type Metadata struct {
	Phase      MetadataPhase `json:"phase"`                 // "planned" or "outcome"
	Attempt    int           `json:"attempt"`               // pool-level attempt number (0-based). Orchestrator emits 0; pool rewrites.
	Path       string        `json:"path"`                  // "buildrequest" or "legacy"
	PathReason string        `json:"path_reason,omitempty"` // legacy classification enum; empty when Path=="buildrequest"

	// BuildRequestAPI identifies which BAML API drove a Path=="buildrequest"
	// request. "request" means the non-streaming Request API (unary HTTP,
	// RunCallOrchestration). "streamrequest" means the streaming StreamRequest
	// API (SSE accumulation, RunStreamOrchestration); for call modes this
	// indicates the bridge from /call{,-with-raw} through stream accumulation,
	// used when the non-streaming API is unavailable for the resolved
	// provider. Empty on the legacy path.
	BuildRequestAPI string `json:"build_request_api,omitempty"`

	// Planned fields
	Client         string   `json:"client,omitempty"`           // resolved runtime client name (strategy or single)
	Provider       string   `json:"provider,omitempty"`         // provider of resolved single client; empty for strategies
	Strategy       string   `json:"strategy,omitempty"`         // e.g. "baml-fallback", "baml-roundrobin"
	Chain          []string `json:"chain,omitempty"`            // child client names for fallback chains
	LegacyChildren []string `json:"legacy_children,omitempty"`  // child names that must go through legacy dispatch
	RetryMax       *int     `json:"retry_max,omitempty"`        // configured max retries
	RetryPolicy    string   `json:"retry_policy,omitempty"`     // compact encoding (e.g. "exp:200ms:1.5:10s")

	// Outcome fields — populated only on the outcome event.
	// RetryCount counts retries consumed by the BuildRequest orchestrator
	// itself (one per attempt of the fallback strategy). Always nil on the
	// legacy path: the legacy path runs no outer retry orchestrator — BAML's
	// runtime owns retry behaviour internally and that count is surfaced via
	// BamlCallCount instead.
	RetryCount     *int   `json:"retry_count,omitempty"`
	WinnerClient   string `json:"winner_client,omitempty"`        // actual client that produced the final result; absent for legacy strategy routes when funcLog.SelectedCall yields no data
	WinnerProvider string `json:"winner_provider,omitempty"`      // provider of the winner
	WinnerPath     string `json:"winner_path,omitempty"`          // "buildrequest" or "legacy" (latter for pure legacy or a legacy child inside a mixed chain)
	UpstreamDurMs  *int64 `json:"upstream_duration_ms,omitempty"` // upstream wall time in ms (orchestrator entry to outcome emission)
	// BamlCallCount is the number of *additional* LLM calls BAML made beyond
	// the first, computed as max(len(FunctionLog.Calls())-1, 0). It collapses
	// BAML-internal retries (per-client retry policy) and BAML-internal
	// fallback chain walking into one figure — nonzero means BAML did
	// something beyond a single happy-path call. Populated only on the
	// legacy path (the BuildRequest path issues one HTTP request per outer
	// attempt and surfaces its retries via RetryCount).
	BamlCallCount *int `json:"baml_call_count,omitempty"`

	// RoundRobin describes the round-robin decision for this request. Non-nil
	// when the resolved client is a baml-roundrobin strategy (at any level of
	// nesting); nil otherwise. The populated Info reflects the OUTERMOST RR
	// decision — inner RR strategies still advance their own counters but are
	// not reported separately.
	RoundRobin *RoundRobinInfo `json:"round_robin,omitempty"`
}

// RoundRobinInfo captures the outcome of resolving a baml-roundrobin strategy
// client for a single request. It carries both the configured chain and the
// child that was picked, so consumers can verify the distribution without
// replaying the decision themselves.
type RoundRobinInfo struct {
	// Name is the client name of the round-robin strategy itself.
	Name string `json:"name"`
	// Children is the ordered list of candidate children considered for
	// this request, after any client_registry strategy override was applied.
	Children []string `json:"children"`
	// Index is the 0-based position in Children that was selected.
	Index int `json:"index"`
	// Selected is Children[Index] — the child dispatched to.
	Selected string `json:"selected"`
}

// StreamMode controls how streaming results are processed and what data is collected.
type StreamMode int

const (
	// StreamModeCall - final only, no raw, no partials (for /call endpoint)
	StreamModeCall StreamMode = iota
	// StreamModeStream - partials + final, no raw (for /stream endpoint)
	StreamModeStream
	// StreamModeCallWithRaw - final + raw, no partials (for /call-with-raw endpoint)
	StreamModeCallWithRaw
	// StreamModeStreamWithRaw - partials + final + raw (for /stream-with-raw endpoint)
	StreamModeStreamWithRaw
)

// NeedsRaw returns true if this mode requires raw LLM response collection.
func (m StreamMode) NeedsRaw() bool {
	return m == StreamModeCallWithRaw || m == StreamModeStreamWithRaw
}

// NeedsPartials returns true if this mode requires forwarding partial/intermediate results.
func (m StreamMode) NeedsPartials() bool {
	return m == StreamModeStream || m == StreamModeStreamWithRaw
}

type StreamingPrompt func(adapter Adapter, input any) (<-chan StreamResult, error)

type StreamingMethod struct {
	MakeInput        func() any
	MakeOutput       func() any
	MakeStreamOutput func() any // Stream/partial type (may differ from final output type)
	Impl             StreamingPrompt
}

type ParsePrompt func(adapter Adapter, raw string) (any, error)

type ParseMethod struct {
	MakeOutput func() any
	Impl       ParsePrompt
}

type ClientRegistry struct {
	Primary *string           `json:"primary"`
	Clients []*ClientProperty `json:"clients"`
}

// ErrDuplicateClientName is the sentinel returned by ClientRegistry.Validate
// when two or more entries in Clients share a Name. BAML's upstream
// AddLlmClient stores entries in a map (last-wins on duplicate names), but
// every baml-rest lookup — primary-provider cache, BuildRequest preflight
// helpers, RR resolver provider/strategy lookups — scans the slice forward
// and returns the FIRST match. Silently picking one over the other lets a
// runtime registry with two `{Name:"Tenant"}` entries (first openai, second
// empty-provider) route under one classification while BAML executes the
// other; classification, support gating, retry-policy resolution, RR
// inspection, and X-BAML-Path-Reason all drift from what BAML actually
// runs. Rejecting at the adapter entry-point surfaces the operator's
// ambiguous input as a hard error rather than letting the divergence mask
// itself.
var ErrDuplicateClientName = errors.New("bamlutils: duplicate client name in client_registry.clients[]")

// ErrEmptyClientName is the sentinel returned by ClientRegistry.Validate
// when an entry in Clients has Name == "". Empty Names are operator typos
// that BAML's AddLlmClient would happily index under "" while every
// baml-rest lookup keys on Name — so the entry effectively becomes a
// nameless ghost the resolver / classifier can't reach but that BAML
// might still execute. Worse, OriginalClientRegistry is captured at the
// top of every adapter's SetClientRegistry, so even a forward-loop skip
// in the BAML-bound views would still let generated scoped-child
// registries (codegen.go BuildLegacyChildRegistryEntries) re-introduce
// the empty-name entry from the original. Rejecting at Validate() is
// the only seam that closes both vectors.
var ErrEmptyClientName = errors.New("bamlutils: empty client name in client_registry.clients[]")

// Validate returns the appropriate sentinel (wrapped with diagnostics)
// when the runtime registry has invariant violations:
//
//   - ErrEmptyClientName when an entry has Name == "". The wrapped
//     error names the slice index so the operator can locate the
//     offending entry. Empty-Name is checked BEFORE duplicates so an
//     entry violating both invariants surfaces as ErrEmptyClientName
//     (the more fundamental shape).
//   - ErrDuplicateClientName when two or more entries share a Name.
//     The wrapped error names the duplicate.
//
// nil entries are skipped — nil represents an operator-tolerable
// absence (a JSON-shaped `clients: [null]` doesn't carry intent), as
// distinct from an empty-Name typo where the operator clearly meant
// to declare a client.
//
// All adapter SetClientRegistry implementations must call Validate
// BEFORE any state mutation so a rejected registry leaves the adapter's
// three views (BuildRequest-safe, legacy, original) unchanged. Returned
// errors wrap their respective sentinels so callers can match with
// errors.Is.
func (r *ClientRegistry) Validate() error {
	if r == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(r.Clients))
	for i, c := range r.Clients {
		if c == nil {
			continue
		}
		if c.Name == "" {
			return fmt.Errorf("%w: client at index %d", ErrEmptyClientName, i)
		}
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateClientName, c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	return nil
}

type ClientProperty struct {
	Name        string         `json:"name"`
	Provider    string         `json:"provider"`
	RetryPolicy *string        `json:"retry_policy"`
	Options     map[string]any `json:"options,omitempty"`

	// ProviderSet records whether the runtime client_registry JSON
	// supplied a `provider` key. The struct-tag-driven decoder collapses
	// "key absent" and "key present with empty string" into the zero
	// value, hiding requests that explicitly clear the provider — a
	// shape BAML upstream rejects in ClientProvider::from_str
	// (clientspec.rs:119-144). Custom UnmarshalJSON below populates
	// this so the resolvers can route present-empty overrides to legacy
	// (where BAML emits its native invalid-provider error) instead of
	// silently using the introspected fallback.
	//
	// Test fixtures using struct literals do not invoke UnmarshalJSON;
	// IsProviderPresent treats Provider != "" as implicit presence so
	// existing fixtures keep working without explicit ProviderSet
	// wiring. To simulate "present-empty" in a struct literal, set
	// {Provider: "", ProviderSet: true}.
	ProviderSet bool `json:"-"`
}

// IsProviderPresent reports whether the runtime client_registry entry
// supplied a `provider` value the resolver should honour. Returns true
// when either Provider is non-empty (struct-literal convenience for
// tests) or ProviderSet is true (JSON-decoded entry that explicitly
// included the `provider` key, even if its value is empty).
//
// Three states the callers care about:
//
//   - !IsProviderPresent(): provider key absent — fall back to the
//     introspected provider (preserves strategy-only and presence-only
//     registry overrides).
//   - IsProviderPresent() && Provider != "": valid override — normalize
//     and use.
//   - IsProviderPresent() && Provider == "": invalid override — caller
//     routes to legacy with PathReasonInvalidProviderOverride so BAML
//     emits its native invalid-provider error.
func (c *ClientProperty) IsProviderPresent() bool {
	if c == nil {
		return false
	}
	return c.Provider != "" || c.ProviderSet
}

// UnmarshalJSON populates ProviderSet from the raw input so callers
// can distinguish "provider key absent" from "provider key present
// with empty string". The struct-tag decoder collapses both into the
// zero value, but BAML upstream rejects empty provider strings, so
// the request resolver needs to route present-empty overrides to
// legacy rather than silently using the introspected provider.
func (c *ClientProperty) UnmarshalJSON(data []byte) error {
	type alias ClientProperty // avoid recursion
	aux := alias{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*c = ClientProperty(aux)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	_, c.ProviderSet = raw["provider"]
	return nil
}

// MarshalJSON preserves the absent vs present-empty distinction
// across a round-trip. The default marshaler emits
// `"provider":""` for both states because Provider is a plain string
// — once unmarshalled and re-marshalled, an originally-absent value
// would re-decode as present-empty and silently route to legacy with
// PathReasonInvalidProviderOverride. Emit the key only when
// IsProviderPresent() reports true; absent + non-empty is impossible
// since IsProviderPresent treats Provider != "" as implicit presence.
//
// The alias type strips ClientProperty's MarshalJSON method to avoid
// infinite recursion. Provider is overlaid as a *string so omitempty
// drops the key on the absent path while keeping `"provider":""` on
// the present-empty path.
func (c ClientProperty) MarshalJSON() ([]byte, error) {
	type alias ClientProperty
	out := struct {
		Provider *string `json:"provider,omitempty"`
		alias
	}{
		alias: alias(c),
	}
	if c.IsProviderPresent() {
		p := c.Provider
		out.Provider = &p
	}
	return json.Marshal(out)
}

// TranslateUpstreamProvider converts baml-rest's canonical provider
// spelling into a form BAML upstream's ClientProvider::from_str
// accepts (clientspec.rs:119-144). Specifically, baml-rest treats
// "baml-roundrobin" as canonical for metadata/headers, but BAML
// upstream only accepts "baml-round-robin" (with the inner hyphen)
// and "round-robin" — sending "baml-roundrobin" verbatim into the
// upstream ClientRegistry triggers a CFFI-time decode failure.
//
// The translation is intentionally narrow: only "baml-roundrobin"
// is rewritten. "baml-round-robin", "round-robin", "fallback",
// "baml-fallback", and every concrete provider all pass through
// unchanged — they are already accepted upstream verbatim. Operator
// spellings that arrive on a registry entry's Provider field are
// preserved when valid; the helper exists solely to avoid translating
// our own canonical naming into a CFFI rejection.
//
// This is the BAML-bound spelling. baml-rest's classification layer
// continues to fold all RR aliases onto "baml-roundrobin" via
// roundrobin.NormalizeProvider for metadata/header consistency; only
// the upstream registry seam invokes this translation.
func TranslateUpstreamProvider(provider string) string {
	if provider == "baml-roundrobin" {
		return "baml-round-robin"
	}
	return provider
}

// IsResolvedStrategyParent reports whether a runtime client_registry
// entry is a baml-rest-resolved RR or fallback strategy parent. These
// entries must not be forwarded into BAML's upstream client registry:
//
//   - Modern adapters (SupportsWithClient=true) resolve strategy
//     parents to a leaf via ResolveEffectiveClient and dispatch with
//     WithClient(leaf). BAML never executes the parent, so forwarding
//     it serves no purpose.
//   - The parent often carries a shape BAML's parser rejects:
//     presence-only `{Name:"MyRR"}` has no `options.strategy`
//     (round_robin.rs:73-83 requires it); strategy-only with a
//     bracketed-string (`options.strategy: "[\"A\",\"B\"]"`) is
//     baml-rest-specific and BAML's ensure_strategy demands an array
//     (helpers.rs:790-829).
//   - The original registry stays untouched so baml-rest's resolver
//     and metadata classifier (and the options.start parser) read
//     the parent verbatim from OriginalClientRegistry.
//
// Spelling: classification uses the operator-supplied provider when
// IsProviderPresent (covering the `provider:"round-robin"` /
// `provider:"baml-roundrobin"` / `provider:"baml-round-robin"` cases),
// or the introspected provider for omitted-provider entries (covering
// strategy-only and presence-only overrides on a static RR client).
// All four RR spellings and both fallback spellings classify here.
func IsResolvedStrategyParent(client *ClientProperty, introspectedProviders map[string]string) bool {
	if client == nil {
		return false
	}
	var provider string
	if client.IsProviderPresent() {
		provider = client.Provider
	} else if introspectedProviders != nil {
		provider = introspectedProviders[client.Name]
	}
	return isStrategyProviderName(provider)
}

// ShouldDropStrategyParentForTopLevelLegacy reports whether a runtime
// client_registry entry should be hidden from the *top-level legacy*
// BAML registry view. Used in tandem with IsResolvedStrategyParent
// (which gates the broader BuildRequest-bound view).
//
// The two views split because BAML's to_clients (client_registry/mod.rs:
// 109) eagerly parses every runtime client per request, so a parent
// shape BAML rejects fails the entire request — not just the dispatch
// of that parent. The BuildRequest-bound view drops every resolved
// strategy parent (baml-rest dispatches WithClient(leaf), so BAML
// never needs the parent), shielding leaf calls from unrelated parent
// shapes. The legacy view must keep parents BAML *can* execute (so
// runtime fallback overrides actually take effect) and parents BAML
// would *reject canonically* (so invalid-strategy / invalid-start
// overrides surface upstream's error rather than silently using static
// config). The only entries we strip from legacy are inert presence-
// only static parents — `{Name:"TestRR"}` with no provider/strategy/
// start — which would re-trigger the original missing-strategy CFFI
// failure if forwarded.
//
// Returns true (drop) only when:
//   - the entry classifies as a resolved strategy parent, and
//   - it has no operator-supplied Provider, and
//   - its Options carry neither `strategy` nor `start`.
//
// Otherwise returns false (keep). An explicit `provider`, `strategy`,
// or `start` on the entry is the operator's signal that they intend
// to drive (or alter) the parent at runtime; forwarding it lets BAML
// honour the override or emit its canonical validation error.
func ShouldDropStrategyParentForTopLevelLegacy(client *ClientProperty, introspectedProviders map[string]string) bool {
	if !IsResolvedStrategyParent(client, introspectedProviders) {
		return false
	}
	if client.IsProviderPresent() {
		return false
	}
	if client.Options != nil {
		if _, ok := client.Options["strategy"]; ok {
			return false
		}
		if _, ok := client.Options["start"]; ok {
			return false
		}
	}
	return true
}

// isStrategyProviderName reports whether p is one of the BAML strategy
// provider spellings (RR or fallback). Inlined here rather than
// imported from bamlutils/buildrequest/roundrobin to avoid a package
// cycle (roundrobin already imports bamlutils).
func isStrategyProviderName(p string) bool {
	switch p {
	case "round-robin", "baml-round-robin", "baml-roundrobin",
		"fallback", "baml-fallback":
		return true
	}
	return false
}

// UpstreamClientRegistryProvider returns the provider string suitable
// for forwarding to BAML's upstream baml.ClientRegistry.AddLlmClient.
// It resolves the three presence states ClientProperty can be in and
// applies the CFFI-bound spelling translation:
//
//   - client.IsProviderPresent() == true: pass client.Provider through
//     TranslateUpstreamProvider. Includes the present-empty case
//     (Provider == "" && ProviderSet); we forward "" so BAML emits
//     its canonical invalid-provider error rather than us masking the
//     operator's typo with an introspected fallback.
//   - client.IsProviderPresent() == false AND introspectedProviders
//     names this client: substitute the introspected provider after
//     translation. This makes strategy-only and presence-only runtime
//     registry entries valid at the BAML CFFI seam — without the
//     materialisation, WithClientRegistry would forward them to
//     upstream and fail end-to-end.
//   - client.IsProviderPresent() == false AND no introspected entry:
//     return "". The operator referred to a name we know nothing about;
//     letting CFFI emit its native error is clearer than synthesising
//     one here, and matches the legacy handling of unknown-name runtime
//     overrides.
//
// The helper does not mutate the input ClientProperty. Callers that
// need the original presence-aware view (the resolver, the metadata
// classifier, the options.start parser) continue to read from the
// adapter's OriginalClientRegistry, which preserves the operator's
// exact input verbatim.
//
func UpstreamClientRegistryProvider(client *ClientProperty, introspectedProviders map[string]string) string {
	if client == nil {
		return ""
	}
	if client.IsProviderPresent() {
		return TranslateUpstreamProvider(client.Provider)
	}
	if introspectedProviders != nil {
		if p := introspectedProviders[client.Name]; p != "" {
			return TranslateUpstreamProvider(p)
		}
	}
	return ""
}

type TypeBuilder struct {
	BamlSnippets []string      `json:"baml_snippets,omitempty"`
	DynamicTypes *DynamicTypes `json:"dynamic_types,omitempty"`
}

func (b *TypeBuilder) Add(input string) {
	b.BamlSnippets = append(b.BamlSnippets, input)
}

// DynamicTypes defines classes and enums to be created via the imperative TypeBuilder API.
// This provides a JSON schema-like structure for defining types programmatically.
//
// # Overview
//
// DynamicTypes allows you to define BAML types at runtime without writing .baml files.
// This is useful for:
//   - Adding fields to existing dynamic classes based on user input
//   - Creating entirely new types at runtime
//   - Adding values to dynamic enums
//
// # Example Usage
//
//	typeBuilder := &bamlutils.TypeBuilder{
//	    DynamicTypes: &bamlutils.DynamicTypes{
//	        Classes: map[string]*bamlutils.DynamicClass{
//	            "DynamicOutput": {
//	                Properties: map[string]*bamlutils.DynamicProperty{
//	                    "name":   {Type: "string"},
//	                    "age":    {Type: "int"},
//	                    "active": {Type: "bool"},
//	                },
//	            },
//	        },
//	        Enums: map[string]*bamlutils.DynamicEnum{
//	            "Priority": {
//	                Values: []*bamlutils.DynamicEnumValue{
//	                    {Name: "HIGH", Alias: "high priority"},
//	                    {Name: "MEDIUM"},
//	                    {Name: "LOW"},
//	                },
//	            },
//	        },
//	    },
//	}
//
// # Supported Types
//
// Primitive types: "string", "int", "float", "bool", "null"
//
// Composite types:
//   - "list": requires "items" field with element type
//   - "optional": requires "inner" field with wrapped type
//   - "map": requires "keys" and "values" fields
//   - "union": requires "oneOf" array with variant types
//
// Literal types:
//   - "literal_string": requires "value" (string)
//   - "literal_int": requires "value" (integer)
//   - "literal_bool": requires "value" (boolean)
//
// References: use "ref" to reference other classes/enums by name
//
// # JSON Schema Example
//
//	{
//	  "classes": {
//	    "Address": {
//	      "properties": {
//	        "street": {"type": "string"},
//	        "city": {"type": "string"},
//	        "zip": {"type": "optional", "inner": {"type": "string"}}
//	      }
//	    },
//	    "Person": {
//	      "description": "A person with an address",
//	      "properties": {
//	        "name": {"type": "string"},
//	        "age": {"type": "int"},
//	        "address": {"ref": "Address"},
//	        "tags": {"type": "list", "items": {"type": "string"}},
//	        "metadata": {"type": "map", "keys": {"type": "string"}, "values": {"type": "string"}}
//	      }
//	    }
//	  },
//	  "enums": {
//	    "Status": {
//	      "values": [
//	        {"name": "ACTIVE", "alias": "active", "description": "Active status"},
//	        {"name": "INACTIVE", "skip": true}
//	      ]
//	    }
//	  }
//	}
type DynamicTypes struct {
	Classes map[string]*DynamicClass `json:"classes,omitempty"`
	Enums   map[string]*DynamicEnum  `json:"enums,omitempty"`
}

// maxTypeDepth is the maximum nesting depth for type references.
// This prevents stack overflow from maliciously deep or circular type definitions.
const maxTypeDepth = 64

// Validate checks the DynamicTypes schema for errors before processing.
// Returns nil if valid, or an error describing the first issue found.
func (dt *DynamicTypes) Validate() error {
	if dt == nil {
		return nil
	}

	// Collect all defined type names for reference validation
	definedTypes := make(map[string]bool)
	for name := range dt.Classes {
		definedTypes[name] = true
	}
	for name := range dt.Enums {
		definedTypes[name] = true
	}

	// Validate enums
	for name, enum := range dt.Enums {
		if enum == nil {
			return fmt.Errorf("enum %q: definition is nil", name)
		}
		for i, v := range enum.Values {
			if v == nil {
				return fmt.Errorf("enum %q: value at index %d is nil", name, i)
			}
			if v.Name == "" {
				return fmt.Errorf("enum %q: value at index %d has empty name", name, i)
			}
		}
	}

	// Validate classes
	for name, class := range dt.Classes {
		if class == nil {
			return fmt.Errorf("class %q: definition is nil", name)
		}
		for propName, prop := range class.Properties {
			if prop == nil {
				return fmt.Errorf("class %q: property %q is nil", name, propName)
			}
			if err := validateTypeRef(prop.Type, prop.Ref, prop.Items, prop.Inner, prop.OneOf, prop.Keys, prop.Values, prop.Value, definedTypes, fmt.Sprintf("class %q property %q", name, propName), 0); err != nil {
				return err
			}
		}
	}

	return nil
}

// validateTypeRef validates a type reference structure
func validateTypeRef(typ, ref string, items, inner *DynamicTypeSpec, oneOf []*DynamicTypeSpec, keys, values *DynamicTypeSpec, value any, definedTypes map[string]bool, path string, depth int) error {
	if depth > maxTypeDepth {
		return fmt.Errorf("%s: type nesting exceeds maximum depth of %d", path, maxTypeDepth)
	}

	// Must have either Type or Ref (or be a literal with Value)
	hasType := typ != ""
	hasRef := ref != ""

	if hasRef {
		if hasType {
			return fmt.Errorf("%s: cannot have both 'type' and 'ref'", path)
		}
		// Reference validation - check if the referenced type is defined
		// Note: we allow references to types not in DynamicTypes as they may exist in baml_src
		return nil
	}

	if !hasType {
		return fmt.Errorf("%s: must have 'type' or 'ref'", path)
	}

	// Validate based on type
	switch typ {
	case "string", "int", "float", "bool", "null":
		// Primitives - no additional fields needed
	case "list":
		if items == nil {
			return fmt.Errorf("%s: 'list' type requires 'items'", path)
		}
		if err := validateDynamicTypeSpec(items, definedTypes, path+"[items]", depth+1); err != nil {
			return err
		}
	case "optional":
		if inner == nil {
			return fmt.Errorf("%s: 'optional' type requires 'inner'", path)
		}
		if err := validateDynamicTypeSpec(inner, definedTypes, path+"[inner]", depth+1); err != nil {
			return err
		}
	case "map":
		if keys == nil || values == nil {
			return fmt.Errorf("%s: 'map' type requires 'keys' and 'values'", path)
		}
		if err := validateDynamicTypeSpec(keys, definedTypes, path+"[keys]", depth+1); err != nil {
			return err
		}
		if err := validateDynamicTypeSpec(values, definedTypes, path+"[values]", depth+1); err != nil {
			return err
		}
	case "union":
		if len(oneOf) == 0 {
			return fmt.Errorf("%s: 'union' type requires 'oneOf' with at least one type", path)
		}
		for i, variant := range oneOf {
			if variant == nil {
				return fmt.Errorf("%s: 'oneOf[%d]' is nil", path, i)
			}
			if err := validateDynamicTypeSpec(variant, definedTypes, fmt.Sprintf("%s[oneOf.%d]", path, i), depth+1); err != nil {
				return err
			}
		}
	case "literal_string":
		if value == nil {
			return fmt.Errorf("%s: 'literal_string' type requires 'value'", path)
		}
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s: 'literal_string' value must be a string", path)
		}
	case "literal_int":
		if value == nil {
			return fmt.Errorf("%s: 'literal_int' type requires 'value'", path)
		}
		// JSON numbers unmarshal as float64
		switch v := value.(type) {
		case int, int64, float64:
			// ok
		default:
			return fmt.Errorf("%s: 'literal_int' value must be an integer, got %T", path, v)
		}
	case "literal_bool":
		if value == nil {
			return fmt.Errorf("%s: 'literal_bool' type requires 'value'", path)
		}
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: 'literal_bool' value must be a boolean", path)
		}
	default:
		return fmt.Errorf("%s: unknown type %q", path, typ)
	}

	return nil
}

// validateDynamicTypeSpec validates a nested DynamicTypeSpec
func validateDynamicTypeSpec(ref *DynamicTypeSpec, definedTypes map[string]bool, path string, depth int) error {
	if ref == nil {
		return fmt.Errorf("%s: type reference is nil", path)
	}
	return validateTypeRef(ref.Type, ref.Ref, ref.Items, ref.Inner, ref.OneOf, ref.Keys, ref.Values, ref.Value, definedTypes, path, depth)
}

// DynamicClass defines a class with properties.
type DynamicClass struct {
	Description string                      `json:"description,omitempty"`
	Alias       string                      `json:"alias,omitempty"`
	Properties  map[string]*DynamicProperty `json:"properties,omitempty"`
}

// DynamicProperty defines a property on a class.
type DynamicProperty struct {
	// Type specification - use Type for primitives/composites, Ref for references
	Type string `json:"type,omitempty"` // "string", "int", "float", "bool", "null", "list", "optional", "map", "union", "literal_string", "literal_int", "literal_bool"
	Ref  string `json:"ref,omitempty"`  // Reference to another class/enum by name

	// Metadata
	Description string `json:"description,omitempty"`
	Alias       string `json:"alias,omitempty"`

	// For composite types
	Items  *DynamicTypeSpec   `json:"items,omitempty"`  // For "list" type
	Inner  *DynamicTypeSpec   `json:"inner,omitempty"`  // For "optional" type
	OneOf  []*DynamicTypeSpec `json:"oneOf,omitempty"`  // For "union" type
	Keys   *DynamicTypeSpec   `json:"keys,omitempty"`   // For "map" type
	Values *DynamicTypeSpec   `json:"values,omitempty"` // For "map" type

	// For literal types
	Value any `json:"value,omitempty"` // For literal_string, literal_int, literal_bool
}

// DynamicTypeSpec is a recursive type specification used in composite types.
// Note: This type was renamed from DynamicTypeRef to avoid a heuristic in
// kin-openapi's openapi3gen that treats types ending in "Ref" with both
// "Ref" and "Value" fields specially, generating incorrect oneOf schemas.
type DynamicTypeSpec struct {
	Type string `json:"type,omitempty"` // Primitive or composite type name
	Ref  string `json:"ref,omitempty"`  // Reference to class/enum

	// For nested composite types
	Items  *DynamicTypeSpec   `json:"items,omitempty"`
	Inner  *DynamicTypeSpec   `json:"inner,omitempty"`
	OneOf  []*DynamicTypeSpec `json:"oneOf,omitempty"`
	Keys   *DynamicTypeSpec   `json:"keys,omitempty"`
	Values *DynamicTypeSpec   `json:"values,omitempty"`
	Value  any                `json:"value,omitempty"`
}

// DynamicEnum defines an enum with values.
// Use this to add new values to existing dynamic enums or create new enums entirely.
type DynamicEnum struct {
	Description string              `json:"description,omitempty"` // Description shown to the LLM
	Alias       string              `json:"alias,omitempty"`       // Alternative name the LLM can use
	Values      []*DynamicEnumValue `json:"values,omitempty"`      // List of enum values
}

// DynamicEnumValue defines a single enum value.
//
// Example with all fields:
//
//	{
//	    "name": "HIGH",
//	    "description": "High priority items that need immediate attention",
//	    "alias": "high priority",
//	    "skip": false
//	}
//
// The Alias field is particularly useful for allowing the LLM to output
// natural language values (like "high priority") that get mapped to
// the canonical enum name ("HIGH").
type DynamicEnumValue struct {
	Name        string `json:"name"`                  // Required: the canonical enum value name
	Description string `json:"description,omitempty"` // Optional: description shown to the LLM
	Alias       string `json:"alias,omitempty"`       // Optional: alternative name the LLM can output
	Skip        bool   `json:"skip,omitempty"`        // Optional: if true, this value is hidden from the LLM
}

// Logger is a minimal logging interface compatible with hclog.Logger.
type Logger interface {
	Debug(msg string, args ...interface{})
	Info(msg string, args ...interface{})
	Warn(msg string, args ...interface{})
	Error(msg string, args ...interface{})
}

// RoundRobinAdvancer returns the next child index for a static baml-
// roundrobin client. Declared here (rather than in the roundrobin
// subpackage) so Adapter can expose a RoundRobinAdvancer accessor
// without a dependency on buildrequest/roundrobin, which would be a
// cycle. The roundrobin package re-exports this as `Advancer` via a
// type alias for call sites that prefer that spelling.
//
// Implementations must return a value in [0, childCount). childCount
// must be > 0; a zero or negative value returns (0, nil) without
// touching state in the existing implementations.
//
// The error return is load-bearing for the remote-advancer case: when
// a pool-managed worker has been attached to a host SharedState socket
// but the round-trip fails (transport error, host crash, etc.), the
// caller must fail the request rather than silently fall back to a
// local random pick. A silent fallback would collapse the pool-wide
// rotation back to per-worker draws exactly when central coordination
// is supposed to kick in — the failure mode the shared-state wiring
// exists to eliminate. In-process implementations always return nil.
type RoundRobinAdvancer interface {
	Advance(clientName string, childCount int) (int, error)
}

type Adapter interface {
	context.Context
	SetClientRegistry(clientRegistry *ClientRegistry) error
	SetTypeBuilder(typeBuilder *TypeBuilder) error
	// SetStreamMode sets the streaming mode which controls partial forwarding and raw collection.
	SetStreamMode(mode StreamMode)
	// StreamMode returns the current streaming mode.
	StreamMode() StreamMode
	// SetLogger sets the logger for debug output.
	SetLogger(logger Logger)
	// Logger returns the current logger, or nil if not set.
	Logger() Logger
	// NewMediaFromURL creates a BAML media object from a URL.
	// Returns the opaque BAML media interface (e.g., baml.Image) as any.
	NewMediaFromURL(kind MediaKind, url string, mimeType *string) (any, error)
	// NewMediaFromBase64 creates a BAML media object from base64-encoded data.
	// Returns the opaque BAML media interface (e.g., baml.Image) as any.
	NewMediaFromBase64(kind MediaKind, base64 string, mimeType *string) (any, error)
	// SetRetryConfig sets the per-request retry configuration override.
	// When set, the BuildRequest path uses this instead of the .baml-introspected policy.
	SetRetryConfig(config *RetryConfig)
	// RetryConfig returns the per-request retry config, or nil if not set.
	RetryConfig() *RetryConfig
	// SetIncludeThinkingInRaw enables provider-specific reasoning/thinking
	// content in the /with-raw `raw` field for this request. Default false
	// matches upstream BAML's RawLLMResponse() semantics (text-only). The
	// flag never affects the parseable text seen by Parse/ParseStream.
	SetIncludeThinkingInRaw(includeThinking bool)
	// IncludeThinkingInRaw returns the per-request opt-in flag. Defaults to
	// false when no override has been applied.
	IncludeThinkingInRaw() bool
	// ClientRegistryProvider returns the provider string of the primary client
	// from the runtime ClientRegistry override, or empty string if no override.
	// Used by the BuildRequest router to select the correct SSE delta extractor.
	ClientRegistryProvider() string
	// OriginalClientRegistry returns the original bamlutils.ClientRegistry
	// that was passed to SetClientRegistry, or nil if no override was set.
	// Used by the BuildRequest router to resolve named client overrides.
	OriginalClientRegistry() *ClientRegistry
	// HTTPClient returns a custom llmhttp.Client for the BuildRequest path,
	// or nil to use llmhttp.DefaultClient. This allows injecting a custom
	// HTTP client (e.g., for testing or proxy support).
	HTTPClient() *llmhttp.Client
	// SetRoundRobinAdvancer installs the per-request Advancer used by the
	// BuildRequest path to resolve baml-roundrobin strategy clients. Pool-
	// managed workers set this to a RemoteAdvancer that talks to the host
	// SharedState service; standalone workers leave it unset and fall back
	// to the package-level Coordinator compiled into introspected.
	SetRoundRobinAdvancer(advancer RoundRobinAdvancer)
	// RoundRobinAdvancer returns the per-request Advancer, or nil if none
	// was installed. Callers treat nil as "use the introspected default".
	RoundRobinAdvancer() RoundRobinAdvancer
}

// BamlOptions contains optional configuration for BAML method calls
type BamlOptions struct {
	ClientRegistry *ClientRegistry `json:"client_registry"`
	TypeBuilder    *TypeBuilder    `json:"type_builder"`
	Retry          *RetryConfig    `json:"retry,omitempty"`
	// IncludeThinkingInRaw opts the request into having provider-specific
	// reasoning/thinking content surfaced in /with-raw's `raw` field.
	//
	// Default false aligns baml-rest with upstream BAML's RawLLMResponse()
	// semantics (text-only). Set true to additionally accumulate Anthropic
	// thinking_delta / thinking blocks (and, in later phases, other providers'
	// reasoning surfaces) into raw for telemetry. The flag never affects the
	// parseable text passed to Parse/ParseStream — that stays text-only by
	// construction so reasoning content cannot influence parsing.
	IncludeThinkingInRaw bool `json:"include_thinking_in_raw,omitempty"`
}

// RetryConfig provides explicit retry configuration for the BuildRequest path.
// When present in __baml_options__.retry, it overrides any .baml-introspected
// retry policy for that request. Zero-value fields use defaults from the retry
// package (200ms delay, 1.5x multiplier, 10s max delay) — see
// buildrequest.RetryConfigToPolicy for the conversion logic.
type RetryConfig struct {
	MaxRetries int     `json:"max_retries"`
	Strategy   string  `json:"strategy,omitempty"`     // "constant_delay" or "exponential_backoff"
	DelayMs    int     `json:"delay_ms,omitempty"`     // Delay in milliseconds (default: 200)
	Multiplier float64 `json:"multiplier,omitempty"`   // For exponential_backoff (default: 1.5)
	MaxDelayMs int     `json:"max_delay_ms,omitempty"` // For exponential_backoff (default: 10000)
}

// HasRetryConfig returns true if the adapter has a per-request retry config.
func HasRetryConfig(adapter Adapter) bool {
	return adapter.RetryConfig() != nil
}

// GetRetryConfig returns the adapter's retry config, or nil.
func GetRetryConfig(adapter Adapter) *RetryConfig {
	return adapter.RetryConfig()
}
