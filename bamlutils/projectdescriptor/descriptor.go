// Package projectdescriptor is the M1 seed of the neutral, whole-project
// descriptor the native codegen lane consumes (codegen-spine decision D1;
// docs/codegen-spine/02-descriptor-ownership-adr.md).
//
// It is passive and dependency-light by construction: it COMPOSES the two
// existing passive descriptors — schemadescriptor.Bundle (the ordered output
// type graph) and promptdescriptor's resolved value/model vocabularies — rather
// than re-declaring their type systems, imports no root/internal runtime (and in
// particular nothing from nativeserve/admission or the internal feature
// packages), and orders all data explicitly, never Go-map order. That
// one-directional dependency is what makes it a safe generated-code boundary:
// generated native carriers can depend on it without pulling BAML or CFFI into
// their graph.
//
// It is also the neutral ARTIFACT that crosses the module boundary between the
// producer (cmd/introspect --native-spine-descriptors) and the consumer
// (adapters/common/codegen), so every field is JSON-serializable and
// round-trips. That is why Method composes the JSON-clean projections of
// promptdescriptor.Function (its ordered resolved arguments, prompt bytes,
// default client/provider, model provenance, and return Bundle) rather than the
// raw Function, whose bamlparser AST carries transient parse state that does not
// round-trip.
//
// M1 carries only the fields the first native method class needs — one static
// LLM function whose first native capability is unary final-call + final-parse.
// The shape grows additively in later milestones; the version fences below are
// exact-equality and fail-closed, exactly as schemadescriptor.Version and
// promptdescriptor.Version are for the descriptors this one composes.
package projectdescriptor

import (
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// Version is the monotonic ProjectDescriptor schema version, enforced by an
// exact-equality fail-closed fence in [Project.Validate]. Bumping it is a
// coordinated, contract-owner-reviewed change (D11).
//
// Version 2 (M2, whole-project source descriptor) adds the project-wide graphs
// every retained method's serving will read — ordered [Client]s (with their
// resolved body/transport config and retry reference), [RetryPolicy]s,
// fallback/round-robin [Strategy] graphs, [Template] (macro) descriptors — and a
// per-method [MethodCapability] manifest that records, for EVERY retained method,
// whether it is native-admitted (and its required capabilities) or which feature
// blocks it. It carries no new carrier vocabulary: stream requirements ride on
// the per-field streaming metadata already in each method's Return
// [schemadescriptor.Bundle] plus the capability manifest.
//
// Version 3 (M3e-A, spine STREAM substrate) adds the additive [ClassStaticStream]
// method class. It is an INTENTIONAL WIRE INCOMPATIBILITY, not a compatible
// extension: under v3 the name [ClassStaticUnary] still means EXACTLY "unary
// final-call + final-parse", so a v2 descriptor must be REJECTED by the exact-version
// fence below rather than read as a v3 one whose class names silently acquired a new
// meaning. Every descriptor JSON/golden fixture moves with the bump.
const Version = 3

// MethodClass names the native capability class a method was admitted into. A
// declined method carries no class (it stays on generated BAML); its reason is
// recorded in [Project.Diagnostics]. M1 defines exactly one admitted class.
type MethodClass string

const (
	// ClassStaticUnary is a static LLM function served as unary final-call +
	// final-parse: a primitive/enum/class/list/optional input and output graph,
	// a text or standard-role-chat prompt, one literal-model OpenAI leaf client,
	// and none of media, dynamic types, BamlSnippets, tool/body options,
	// fallback/round-robin strategies, checks, or asserts. Any of those leaves
	// the method declined — see the classifier in internal/nativespine.
	//
	// Its contract is FROZEN at exactly that: it promises unary final-call and
	// final-parse and NOTHING about streaming. M3e-A deliberately did NOT broaden it —
	// see [ClassStaticStream].
	ClassStaticUnary MethodClass = "static_unary"

	// ClassStaticStream (Version 3, M3e-A) is a static LLM function served as unary
	// final-call + final-parse AND the two real streaming modes (/stream,
	// /stream-with-raw) + stream parse. It is a SUPERSET of ClassStaticUnary's promise
	// by construction: the same generated method must keep serving /call in the
	// native-only runtime and stay usable by the unary standard composite.
	//
	// It is stamped ONLY on a method the one root-owned totality predicate
	// (internal/debaml.SupportsNativeStaticStreamBundle — the exact five-arm `JSON`
	// recursive alias) admits, on top of every ClassStaticUnary check. It promises
	// NOTHING beyond that: not /call-with-raw, not strategies or retries, not stream
	// annotations, not JsonValue, not arbitrary schemas.
	ClassStaticStream MethodClass = "static_stream"
)

// MethodClasses returns every MethodClass this descriptor version defines, in a
// stable order. It is the enumeration a fail-closed consumer validates an unknown
// class against; [Project.Validate] uses it so a v3 descriptor carrying a class this
// build does not know is a hard error rather than a silently-served method.
func MethodClasses() []MethodClass {
	return []MethodClass{ClassStaticUnary, ClassStaticStream}
}

// IsKnown reports whether c is one of the classes this descriptor version defines.
func (c MethodClass) IsKnown() bool {
	for _, k := range MethodClasses() {
		if c == k {
			return true
		}
	}
	return false
}

// CapabilityCode is an opaque, stable, machine-readable feature/decline code.
// The catalogue of valid values is owned by internal/codegenspine/manifest.json
// (docs/codegen-spine/02-descriptor-ownership-adr.md, "Capability / decline code
// ownership"). This package carries codes as opaque strings only — it defines no
// catalogue and no behavior keyed on a value — which keeps the D1 boundary
// intact. A test in the root module validates that every code this build can
// emit is in the manifest catalogue.
type CapabilityCode string

// Project is the top-level whole-project descriptor and the neutral cross-module
// artifact. Ordering is data: Methods and Diagnostics are source-derived,
// deterministically ordered slices, never Go-map iteration order.
type Project struct {
	// Version is this descriptor's own schema version.
	Version int `json:"version"`
	// PromptDescriptorVersion records the promptdescriptor.Version the composed
	// argument/model projections were produced with; SchemaVersion does the same
	// for the schemadescriptor.Bundle in each method's Return. Both are fenced
	// fail-closed by Validate, giving the ADR's "independent fences" property
	// without embedding the raw descriptors.
	PromptDescriptorVersion int `json:"prompt_descriptor_version"`
	SchemaVersion           int `json:"schema_version"`

	Methods []Method `json:"methods"`

	// Clients, RetryPolicies, and Strategies are the whole-project client graph
	// (Version 2+), each in deterministic source-declaration order. A serving lane
	// reads them to reproduce client selection, retry, and fallback/round-robin
	// behavior; they are descriptor data only (no serving path consumes them yet).
	Clients       []Client      `json:"clients,omitempty"`
	RetryPolicies []RetryPolicy `json:"retry_policies,omitempty"`
	Strategies    []Strategy    `json:"strategies,omitempty"`
	// Templates is the project template_string (macro) set (Version 2+), ordered
	// by template name for determinism (independent of file-walk order), JSON-clean
	// (see [Template]). BAML's render-time macro-injection order is a source-order
	// concern a later render phase recovers; it is deliberately NOT the descriptor
	// order.
	Templates []Template `json:"templates,omitempty"`
	// Capabilities is the per-method native-capability manifest (Version 2+): one
	// record for EVERY retained method (admitted or declined), ordered by method
	// name. It is the seam M4's "a retained endpoint missing from both the native
	// registry and an allowed transition fallback fails the build" rule reads.
	Capabilities []MethodCapability `json:"capabilities,omitempty"`

	Diagnostics []Decline `json:"diagnostics,omitempty"`
}

// Client is one declared `client<llm>` block, in source-declaration order, with
// its resolved body/transport configuration and its retry-policy reference. It
// COMPOSES promptdescriptor.ClientConfig verbatim (the ordered model +
// request_body tree + transport/body-affecting option split) and adds only the
// retry reference the config does not carry. A strategy wrapper client (provider
// baml-fallback / round-robin) also appears here; its ordered children are in the
// matching [Strategy].
type Client struct {
	Config      promptdescriptor.ClientConfig `json:"config"`
	RetryPolicy string                        `json:"retry_policy,omitempty"`
}

// StrategyKind names a client-selection strategy: an ordered fallback chain or a
// round-robin rotation.
type StrategyKind string

const (
	StrategyFallback   StrategyKind = "fallback"
	StrategyRoundRobin StrategyKind = "round_robin"
)

// Strategy is one fallback or round-robin wrapper client and its ordered child
// client names. Start is the round-robin `start N` seed when the block declares
// one (nil otherwise, and always nil for a fallback strategy).
type Strategy struct {
	Name     string       `json:"name"`
	Kind     StrategyKind `json:"kind"`
	Children []string     `json:"children"`
	Start    *int         `json:"start,omitempty"`
}

// RetryPolicy is one declared `retry_policy` block, in source-declaration order.
// Strategy is the delay strategy ("constant_delay" or "exponential_backoff"); the
// delay parameters are the raw source values (Multiplier/MaxDelayMs are only
// meaningful for exponential_backoff).
type RetryPolicy struct {
	Name       string  `json:"name"`
	MaxRetries int     `json:"max_retries"`
	Strategy   string  `json:"strategy,omitempty"`
	DelayMs    int     `json:"delay_ms,omitempty"`
	Multiplier float64 `json:"multiplier,omitempty"`
	MaxDelayMs int     `json:"max_delay_ms,omitempty"`
}

// Template is a retained `template_string` (macro) declaration, JSON-clean.
// Unlike promptdescriptor.TemplateString — whose argument types are transient
// bamlparser AST that does not round-trip — it projects only the argument NAMES
// (a macro argument is never bound to a resolved value type). Body is the raw
// template body with only the raw-string delimiters removed, byte-for-byte. The
// source path is deliberately NOT carried: it is diagnostic-only (not load-
// bearing) and would make the descriptor depend on file names.
type Template struct {
	Name string   `json:"name"`
	Args []string `json:"args,omitempty"`
	Body string   `json:"body"`
}

// MethodCapability is the per-method native-capability record. Every retained
// method has exactly one: an admitted method carries its [MethodClass] and the
// ordered capability codes it requires; a declined method carries the single
// blocking code. Admitted and Blocked are mutually exclusive by construction.
type MethodCapability struct {
	Method   string           `json:"method"`
	Admitted bool             `json:"admitted"`
	Class    MethodClass      `json:"class,omitempty"`
	Required []CapabilityCode `json:"required,omitempty"`
	Blocked  CapabilityCode   `json:"blocked,omitempty"`
}

// Method is one admitted native method. It composes the JSON-clean projections
// of promptdescriptor.Function: Return is a schemadescriptor.Bundle verbatim,
// Args reuse promptdescriptor.ResolvedValueType verbatim, and Model.Provenance
// reuses promptdescriptor.ModelProvenance verbatim; Name/Class/Prompt/Client/
// Provider are the scalar facts. Nothing here re-declares a type vocabulary.
type Method struct {
	Name     string      `json:"name"`
	Class    MethodClass `json:"class"`
	Prompt   string      `json:"prompt"`
	Args     []Argument  `json:"args"`
	Client   string      `json:"client"`
	Provider string      `json:"provider"`
	Model    Model       `json:"model"`
	// Return is the final output schema, composed verbatim from schemadescriptor.
	Return               schemadescriptor.Bundle `json:"return"`
	RequiredCapabilities []CapabilityCode        `json:"required_capabilities,omitempty"`
}

// Argument is one ordered input argument: its name and its resolved value type,
// reused verbatim from promptdescriptor (the V3 resolved input value graph — a
// named graph edge, never a copied class/enum definition).
type Argument struct {
	Name string                             `json:"name"`
	Type promptdescriptor.ResolvedValueType `json:"type"`
}

// Model is the projected default-client model: its resolved value string and the
// provenance (only [promptdescriptor.ModelProvenanceLiteral] is admissible for
// ClassStaticUnary).
type Model struct {
	Value      string                           `json:"value"`
	Provenance promptdescriptor.ModelProvenance `json:"provenance"`
}

// Decline records a method left on generated BAML and the stable, opaque code
// for why. A descriptor may still exist for a declined method — native codegen
// must never infer support from mere presence (manifest "declines.principle").
type Decline struct {
	Method string         `json:"method"`
	Code   CapabilityCode `json:"code"`
	Detail string         `json:"detail,omitempty"`
}

// Validate applies the fail-closed version fences and structural checks. It
// fences three independent versions — the project version, the composed
// promptdescriptor version, and the schemadescriptor version on every admitted
// method's Return — each exact-equality; a mismatch is an error, never a silent
// under-read.
func (p *Project) Validate() error {
	if p.Version != Version {
		return fmt.Errorf("projectdescriptor: unsupported project version %d (want %d)", p.Version, Version)
	}
	if p.PromptDescriptorVersion != promptdescriptor.Version {
		return fmt.Errorf("projectdescriptor: composed prompt-descriptor version %d (want %d)", p.PromptDescriptorVersion, promptdescriptor.Version)
	}
	if p.SchemaVersion != schemadescriptor.Version {
		return fmt.Errorf("projectdescriptor: composed schema version %d (want %d)", p.SchemaVersion, schemadescriptor.Version)
	}
	for i := range p.Methods {
		m := &p.Methods[i]
		if m.Name == "" {
			return fmt.Errorf("projectdescriptor: method[%d] has empty name", i)
		}
		if m.Class == "" {
			return fmt.Errorf("projectdescriptor: method %q has empty class", m.Name)
		}
		// FAIL CLOSED on an unknown class (Version 3). A consumer that silently
		// accepted one would serve a method whose capability promise this build cannot
		// read — exactly the "never infer support from mere presence" rule the
		// capability manifest encodes.
		if !m.Class.IsKnown() {
			return fmt.Errorf("projectdescriptor: method %q has unknown class %q (known: %v)", m.Name, m.Class, MethodClasses())
		}
		if m.Return.Version != schemadescriptor.Version {
			return fmt.Errorf("projectdescriptor: method %q return-schema version %d (want %d)", m.Name, m.Return.Version, schemadescriptor.Version)
		}
	}
	for i := range p.Diagnostics {
		if p.Diagnostics[i].Method == "" {
			return fmt.Errorf("projectdescriptor: diagnostic[%d] has empty method", i)
		}
		if p.Diagnostics[i].Code == "" {
			return fmt.Errorf("projectdescriptor: diagnostic[%d] (method %q) has empty code", i, p.Diagnostics[i].Method)
		}
	}
	seenClient := make(map[string]bool, len(p.Clients))
	for i := range p.Clients {
		name := p.Clients[i].Config.Name
		if name == "" {
			return fmt.Errorf("projectdescriptor: client[%d] has empty name", i)
		}
		if seenClient[name] {
			return fmt.Errorf("projectdescriptor: client %q is declared more than once", name)
		}
		seenClient[name] = true
	}
	seenRetry := make(map[string]bool, len(p.RetryPolicies))
	for i := range p.RetryPolicies {
		name := p.RetryPolicies[i].Name
		if name == "" {
			return fmt.Errorf("projectdescriptor: retry_policy[%d] has empty name", i)
		}
		if seenRetry[name] {
			return fmt.Errorf("projectdescriptor: retry_policy %q is declared more than once", name)
		}
		seenRetry[name] = true
	}
	seenStrategy := make(map[string]bool, len(p.Strategies))
	for i := range p.Strategies {
		s := &p.Strategies[i]
		if s.Name == "" {
			return fmt.Errorf("projectdescriptor: strategy[%d] has empty name", i)
		}
		if s.Kind != StrategyFallback && s.Kind != StrategyRoundRobin {
			return fmt.Errorf("projectdescriptor: strategy %q has invalid kind %q", s.Name, s.Kind)
		}
		if seenStrategy[s.Name] {
			return fmt.Errorf("projectdescriptor: strategy %q is declared more than once", s.Name)
		}
		seenStrategy[s.Name] = true
	}
	seenTemplate := make(map[string]bool, len(p.Templates))
	for i := range p.Templates {
		if p.Templates[i].Name == "" {
			return fmt.Errorf("projectdescriptor: template[%d] has empty name", i)
		}
		if seenTemplate[p.Templates[i].Name] {
			return fmt.Errorf("projectdescriptor: template %q is declared more than once", p.Templates[i].Name)
		}
		seenTemplate[p.Templates[i].Name] = true
	}
	// The capability manifest must cover EVERY retained method exactly once and
	// agree with that method's admit/decline outcome — it is the seam M4 reads, so
	// an absent, duplicate, unknown-method, or inconsistent record is a hard error.
	// A retained method is admitted (in Methods) or declined (in Diagnostics).
	// Admitted (Methods) and declined (Diagnostics) name sets must each be free of
	// duplicates and be DISJOINT — a method is admitted XOR declined, never both,
	// never twice.
	admitted := make(map[string]*Method, len(p.Methods))
	for i := range p.Methods {
		m := &p.Methods[i]
		if _, dup := admitted[m.Name]; dup {
			return fmt.Errorf("projectdescriptor: admitted method %q is declared more than once", m.Name)
		}
		admitted[m.Name] = m
	}
	declined := make(map[string]*Decline, len(p.Diagnostics))
	for i := range p.Diagnostics {
		d := &p.Diagnostics[i]
		name := d.Method
		if declined[name] != nil {
			return fmt.Errorf("projectdescriptor: declined method %q appears in diagnostics more than once", name)
		}
		declined[name] = d
		if _, both := admitted[name]; both {
			return fmt.Errorf("projectdescriptor: method %q is both admitted and declined", name)
		}
	}
	seenCap := make(map[string]bool, len(p.Capabilities))
	for i := range p.Capabilities {
		c := &p.Capabilities[i]
		if c.Method == "" {
			return fmt.Errorf("projectdescriptor: capability[%d] has empty method", i)
		}
		if seenCap[c.Method] {
			return fmt.Errorf("projectdescriptor: duplicate capability record for method %q", c.Method)
		}
		seenCap[c.Method] = true
		m, isAdmitted := admitted[c.Method]
		decline, isDeclined := declined[c.Method]
		if !isAdmitted && !isDeclined {
			return fmt.Errorf("projectdescriptor: capability for unknown method %q (not in methods or diagnostics)", c.Method)
		}
		if c.Admitted != isAdmitted {
			return fmt.Errorf("projectdescriptor: capability for method %q says admitted=%v but the method is %s", c.Method, c.Admitted, admitState(isAdmitted))
		}
		if c.Admitted {
			if c.Class == "" {
				return fmt.Errorf("projectdescriptor: capability for admitted method %q has empty class", c.Method)
			}
			if c.Class != m.Class {
				return fmt.Errorf("projectdescriptor: capability for admitted method %q has class %q but the method's class is %q", c.Method, c.Class, m.Class)
			}
			if c.Blocked != "" {
				return fmt.Errorf("projectdescriptor: capability for admitted method %q also carries a blocked code %q", c.Method, c.Blocked)
			}
			if !equalCapabilityCodes(c.Required, m.RequiredCapabilities) {
				return fmt.Errorf("projectdescriptor: capability for admitted method %q: required %v disagrees with method.RequiredCapabilities %v", c.Method, c.Required, m.RequiredCapabilities)
			}
		} else {
			if c.Blocked == "" {
				return fmt.Errorf("projectdescriptor: capability for declined method %q has empty blocked code", c.Method)
			}
			// The blocking code MUST agree with the method's diagnostic code — the
			// descriptor must never carry two conflicting reasons for one decline.
			if c.Blocked != decline.Code {
				return fmt.Errorf("projectdescriptor: capability for declined method %q has blocked code %q but its diagnostic code is %q", c.Method, c.Blocked, decline.Code)
			}
			// A declined record carries ONLY the blocking code — no admitted-only
			// fields.
			if c.Class != "" {
				return fmt.Errorf("projectdescriptor: capability for declined method %q carries a class %q (admitted-only)", c.Method, c.Class)
			}
			if len(c.Required) != 0 {
				return fmt.Errorf("projectdescriptor: capability for declined method %q carries required capabilities %v (admitted-only)", c.Method, c.Required)
			}
		}
	}
	for name := range admitted {
		if !seenCap[name] {
			return fmt.Errorf("projectdescriptor: admitted method %q has no capability record", name)
		}
	}
	for name := range declined {
		if !seenCap[name] {
			return fmt.Errorf("projectdescriptor: declined method %q has no capability record", name)
		}
	}
	return nil
}

func admitState(admitted bool) string {
	if admitted {
		return "admitted"
	}
	return "declined"
}

// equalCapabilityCodes reports whether two capability-code slices are equal in
// order and content (nil and empty are equal).
func equalCapabilityCodes(a, b []CapabilityCode) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
