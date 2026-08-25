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
const Version = 1

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
	ClassStaticUnary MethodClass = "static_unary"
)

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

	Methods     []Method  `json:"methods"`
	Diagnostics []Decline `json:"diagnostics,omitempty"`
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
	return nil
}
