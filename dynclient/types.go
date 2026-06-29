package dynclient

import (
	"encoding/json"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
	"github.com/invakid404/baml-rest/workerplugin"
)

// Request mirrors the JSON body accepted by the dynamic /call,
// /call-with-raw, /stream, and /stream-with-raw endpoints. Each request
// must carry messages, a client_registry whose primary is set, and an
// output_schema with at least one property. Validate enforces the same
// constraints the HTTP endpoints enforce.
type Request = bamlutils.DynamicInput

// ParseRequest mirrors the JSON body accepted by the dynamic /parse
// endpoint. Raw is the LLM output to be parsed; output_schema describes
// the expected structure.
type ParseRequest = bamlutils.DynamicParseInput

// Message is a chat message with role, content, and optional metadata.
// Content can be a plain string or an array of typed parts; see the
// upstream bamlutils.DynamicMessage doc for the union shape.
type Message = bamlutils.DynamicMessage

// ContentPart is a single typed part within a multi-part message
// content array.
type ContentPart = bamlutils.DynamicContentPart

// MediaInput is the JSON-friendly representation of a BAML media value
// (image, audio, pdf, video) inside a message part.
type MediaInput = bamlutils.MediaInput

// CacheControl carries Anthropic prompt-caching metadata for a message.
type CacheControl = bamlutils.CacheControl

// MessageMetadata holds optional per-message metadata (e.g. cache_control).
type MessageMetadata = bamlutils.MessageMetadata

// OutputSchema describes the output structure for a dynamic request,
// supporting both simple flat schemas and nested class/enum definitions.
type OutputSchema = bamlutils.DynamicOutputSchema

// Property describes a single field in an output schema or class.
type Property = bamlutils.DynamicProperty

// TypeSpec is the recursive type specification used by nested fields
// (list items, optional inner, map keys/values, union variants).
type TypeSpec = bamlutils.DynamicTypeSpec

// Class describes a class type referenced by an output schema.
type Class = bamlutils.DynamicClass

// Enum describes an enum type referenced by an output schema.
type Enum = bamlutils.DynamicEnum

// EnumValue is a single value entry inside an Enum.
type EnumValue = bamlutils.DynamicEnumValue

// ClientRegistry overrides the LLM client configuration for a request.
type ClientRegistry = bamlutils.ClientRegistry

// ClientProperty defines a single named client inside a ClientRegistry.
type ClientProperty = bamlutils.ClientProperty

// TypeBuilder lets callers inject dynamic types or raw BAML snippets.
type TypeBuilder = bamlutils.TypeBuilder

// DynamicTypes groups dynamic class and enum definitions.
type DynamicTypes = bamlutils.DynamicTypes

// OrderedMap is the insertion-ordered map type used by Property /
// Class / Enum maps inside an OutputSchema. Re-exported from bamlutils
// so dynclient callers do not need a second import.
type OrderedMap[V any] = bamlutils.OrderedMap[V]

// OrderedEntry is the (key, value) pair type accepted by NewOrderedMap
// and MustOrderedMap.
type OrderedEntry[V any] = bamlutils.OrderedEntry[V]

// OrderedKV constructs an OrderedEntry for use in NewOrderedMap /
// MustOrderedMap argument lists.
func OrderedKV[V any](key string, value V) OrderedEntry[V] {
	return bamlutils.OrderedKV(key, value)
}

// NewOrderedMap builds an insertion-ordered map from entries; duplicate
// keys are rejected.
func NewOrderedMap[V any](entries ...OrderedEntry[V]) (OrderedMap[V], error) {
	return bamlutils.NewOrderedMap(entries...)
}

// MustOrderedMap is the panicking variant of NewOrderedMap intended for
// tests and trusted package-local construction.
func MustOrderedMap[V any](entries ...OrderedEntry[V]) OrderedMap[V] {
	return bamlutils.MustOrderedMap(entries...)
}

// RetryConfig provides explicit per-request retry configuration.
type RetryConfig = bamlutils.RetryConfig

// Metadata is the routing/retry payload emitted by the orchestrator
// alongside call and stream results.
type Metadata = bamlutils.Metadata

// MetadataPhase identifies the two metadata phases (planned, outcome).
type MetadataPhase = bamlutils.MetadataPhase

// RoundRobinInfo captures the round-robin decision recorded in Metadata.
type RoundRobinInfo = bamlutils.RoundRobinInfo

// BuildRequestConfig carries the per-client BuildRequest toggles. Public
// callers configure these through WithDisableCallBuildRequest rather than
// constructing this struct directly. The BuildRequest route itself is
// unconditional as of #537.
type BuildRequestConfig = bamlutils.BuildRequestConfig

// Logger is the minimal logger interface accepted by WithLogger; the
// shape is compatible with hclog.Logger.
type Logger = bamlutils.Logger

// BaseURLRewriteRule rewrites a client base_url before it is handed to
// the underlying LLM HTTP client.
type BaseURLRewriteRule = urlrewrite.Rule

// SharedStateStore is the host-side store used to coordinate
// baml-roundrobin counters across concurrent requests within a single
// process. Callers create one with workerplugin.NewSharedStateStore and
// pass it through WithSharedStateStore.
type SharedStateStore = workerplugin.SharedStateStore

// CallResult is the result of a non-streaming dynamic call. Data is the
// flattened JSON payload (no DynamicProperties wrapper). Metadata
// carries the planned/outcome routing events the orchestrator emitted
// alongside the final answer, when any were produced.
type CallResult struct {
	Data     json.RawMessage
	Metadata []Metadata
}

// CallRawResult is the result of a non-streaming dynamic call that also
// captures the raw and reasoning channels. Raw is the upstream LLM
// response text; Reasoning is the provider-specific thinking/reasoning
// text when the request opted in.
type CallRawResult struct {
	Data      json.RawMessage
	Raw       string
	Reasoning string
	Metadata  []Metadata
}

// ParseResult is the result of a dynamic parse call. Data is the
// flattened JSON payload.
type ParseResult struct {
	Data json.RawMessage
}
