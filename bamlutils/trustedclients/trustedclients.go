// Package trustedclients is the DEPLOYMENT-OWNED approved-configuration
// declaration for the de-BAML serving cutover, and the sealing pass that applies
// it (serving cutover S3a).
//
// # Why this exists
//
// The dynamic `/call` surface takes its `client_registry` from the public request
// body, so nothing derived from that registry can establish that a request is
// running the deployment's approved configuration — see bamlutils/trustedconfig.go
// for the full argument. A configuration IDENTITY therefore needs a fact the
// request cannot manufacture, and this package is where a deployment states it.
//
// # The declaration
//
// A deployment sets [EnvVar] to a small JSON document naming the configuration
// classes it approves, each with the opaque fingerprint it assigned:
//
//	{"trusted_clients":[
//	  {"name":"Approved","fingerprint":"cfg001","provider":"openai",
//	   "options":{"model":"gpt-4o-mini","base_url":"https://api.example/v1","api_key":"sk-…"}}
//	]}
//
// Absent or empty means "no configuration is approved", which is the shipped
// default and what every current deployment runs: nothing is ever sealed, so
// nothing ever obtains an identity.
//
// # The sealing rule — NAMING is allowed, DEFINING is not
//
// [Set.Seal] walks a request's registry and seals a client ONLY when the request
// did no more than NAME it: no provider, no options, no retry policy. It then
// installs the declared provider and options itself and records the seal. A
// request that supplied ANY part of the configuration — including values identical
// to the declared ones — is NOT sealed and is left exactly as it arrived, because
// "the caller sent the right bytes" is not the same fact as "the deployment owns
// this configuration", and only the second one is an identity.
//
// So a caller may CHOOSE among the deployment's approved configurations by name;
// it can never DEFINE one. That is the whole provenance boundary, and
// TestCallerSuppliedConfigurationIsNeverSealed is the standing guard on it.
//
// # Errors are BOUNDED and VALUE-FREE
//
// A declaration is deployment configuration, and it holds the deployment's real
// model, endpoint and credential. So a REJECTED declaration must not echo any of
// it: [Parse] returns a [DeclarationError] carrying only the record INDEX and a
// bounded reason code from a closed set, never a declared name, fingerprint,
// provider, option key or option value, and never the raw declaration bytes. The
// boot paths log that error verbatim, so the redaction has to live here rather
// than at each logger.
//
// The index is the operator's handle: "record 2 has a fingerprint that is not of
// the opaque form" is actionable without publishing what record 2 says.
// TestDeclarationErrorsAreValueFree drives hostile values through every rejection
// path and requires none of them to survive into the error.
//
// # It is not an activation mechanism
//
// Sealing assigns an opaque bucket. It does not permit a native claim: that
// remains the immutable compile-time cohort enrollment's answer, and it is empty.
// A deployment may declare every class it likes and every request still declines.
package trustedclients

import (
	"bytes"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils"
)

// EnvVar is the environment variable a deployment declares its approved
// configuration classes in.
const EnvVar = "BAML_REST_DEBAML_TRUSTED_CLIENTS"

// Reason is the CLOSED set of bounded reason codes a rejected declaration can
// carry. A reason names the RULE that was broken, never the value that broke it.
type Reason string

const (
	ReasonInvalidJSON              Reason = "invalid_json"
	ReasonUnknownField             Reason = "unknown_field"
	ReasonTrailingContent          Reason = "trailing_content"
	ReasonTooManyClients           Reason = "too_many_clients"
	ReasonNameEmpty                Reason = "name_empty"
	ReasonNameNotTrimmed           Reason = "name_not_trimmed"
	ReasonNameTooLong              Reason = "name_too_long"
	ReasonFingerprintNotOpaque     Reason = "fingerprint_not_opaque"
	ReasonProviderEmpty            Reason = "provider_empty"
	ReasonOptionsEmpty             Reason = "options_empty"
	ReasonTooManyOptions           Reason = "too_many_options"
	ReasonOptionKeyEmpty           Reason = "option_key_empty"
	ReasonOptionValueEmpty         Reason = "option_value_empty"
	ReasonNameDeclaredTwice        Reason = "name_declared_twice"
	ReasonFingerprintDeclaredTwice Reason = "fingerprint_declared_twice"
)

// AllReasons returns the closed reason set in declaration order. It is the single
// source of truth the redaction test enumerates.
func AllReasons() []Reason {
	return []Reason{
		ReasonInvalidJSON, ReasonUnknownField, ReasonTrailingContent, ReasonTooManyClients,
		ReasonNameEmpty, ReasonNameNotTrimmed, ReasonNameTooLong, ReasonFingerprintNotOpaque,
		ReasonProviderEmpty, ReasonOptionsEmpty, ReasonTooManyOptions, ReasonOptionKeyEmpty,
		ReasonOptionValueEmpty, ReasonNameDeclaredTwice, ReasonFingerprintDeclaredTwice,
	}
}

// envelopeRecord is the Record value for a failure that is not attributable to one
// record (a malformed document, an unknown envelope key, an over-cap set).
const envelopeRecord = -1

// DeclarationError is a rejected declaration, described WITHOUT quoting any part of
// it. It carries the record index — the operator's handle into their own file — and
// a bounded [Reason]; nothing else. It never wraps the underlying decoder error,
// because a decoder error echoes the input it choked on.
type DeclarationError struct {
	// Record is the 0-based index of the offending record, or -1 when the failure
	// belongs to the document rather than to a record.
	Record int
	// Reason is the bounded rule that was broken.
	Reason Reason
}

func (e *DeclarationError) Error() string {
	if e.Record < 0 {
		return fmt.Sprintf("%s: declaration rejected (%s)", EnvVar, e.Reason)
	}
	return fmt.Sprintf("%s: declaration rejected at record %d (%s)", EnvVar, e.Record, e.Reason)
}

// declErr builds a bounded declaration error.
func declErr(record int, reason Reason) error {
	return &DeclarationError{Record: record, Reason: reason}
}

// MaxClients caps the declared set. It bounds both the label space a declaration
// can reach downstream and the work the sealing pass does per request.
const MaxClients = 16

// maxNameLen bounds a declared client name. Names are hand-written declaration
// entries, so this is a sanity fence rather than a truncation policy.
const maxNameLen = 128

// fingerprintForm is the OPAQUE form a declared fingerprint must take: the literal
// `cfg` followed by three to six decimal digits.
//
// Digits-only after the prefix is the point. A form that cannot carry letters
// cannot carry a model name, a client name, a host label or a credential — so the
// value a deployment assigns is structurally incapable of being an observation,
// whatever it is later used to label. The consumer additionally requires the
// fingerprint to be in ITS OWN declared vocabulary, so this form check is a
// necessary condition and never a sufficient one.
var fingerprintForm = regexp.MustCompile(`^cfg[0-9]{3,6}$`)

// Client is one approved configuration class, exactly as the deployment declared
// it. Every field is deployment input; nothing here is ever taken from a request.
type Client struct {
	// Name is the client name a request may select this configuration by.
	Name string `json:"name"`
	// Fingerprint is the opaque configuration bucket assigned to this class.
	Fingerprint string `json:"fingerprint"`
	// Provider is the provider spelling installed on the sealed client.
	Provider string `json:"provider"`
	// Options are the literal option values installed on the sealed client — the
	// real `model`, `base_url` and `api_key` of the approved configuration.
	//
	// SENSITIVE: this map holds the deployment's credential. It is never logged,
	// never published, and never digested by value (see
	// bamlutils.TrustedConfigDigest).
	Options map[string]string `json:"options"`
}

// Set is the immutable, validated declaration. A nil *Set is a valid EMPTY
// declaration whose Seal is a no-op — the fail-closed default a forgotten wiring
// gets.
type Set struct {
	byName map[string]Client
}

// envelope is the top-level shape of the declaration. Unknown keys are rejected so
// a typo fails loudly instead of silently declaring nothing.
type envelope struct {
	TrustedClients []Client `json:"trusted_clients"`
}

// Load reads [EnvVar] and returns a validated, non-nil *Set. An empty or unset
// variable returns an empty Set and no error.
//
// A malformed declaration is an ERROR, not a degraded empty set: a worker that
// silently approved nothing because someone fat-fingered a key would leave
// operators believing a configuration was approved when it was not. The error is a
// bounded [DeclarationError] and quotes nothing from the declaration.
func Load() (*Set, error) {
	return Parse(os.Getenv(EnvVar))
}

// Parse builds a Set from a raw JSON declaration, so a programmatic caller can
// supply one without going through the environment.
func Parse(raw string) (*Set, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &Set{byName: map[string]Client{}}, nil
	}
	dec := stdjson.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var env envelope
	if err := dec.Decode(&env); err != nil {
		// The decoder's own message quotes the input it choked on — an unknown key,
		// a mistyped value, a fragment of the document. It is CLASSIFIED here and
		// then DISCARDED; only the bounded reason survives.
		return nil, declErr(envelopeRecord, classifyDecodeError(err))
	}
	// Require a real EOF after the first value, not merely "no further ELEMENT".
	//
	// Decoder.More reports whether another element follows in the current array or
	// object, and it deliberately answers false when the next byte is `]` or `}`. At
	// the TOP level that is the wrong question: `{...}}` and `{...}]` have no further
	// element, so More() said "clean" and a declaration with a stray closing brace was
	// ACCEPTED as a valid prefix. A declaration is deployment configuration, and one
	// the operator did not finish writing must fail closed rather than run as whatever
	// prefix happened to parse.
	//
	// Token() after the top-level value returns io.EOF only when nothing but
	// whitespace remains; a stray delimiter returns a syntax error and a second value
	// returns its opening token. Both are trailing content.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, declErr(envelopeRecord, ReasonTrailingContent)
	}
	if len(env.TrustedClients) > MaxClients {
		return nil, declErr(envelopeRecord, ReasonTooManyClients)
	}
	set := &Set{byName: make(map[string]Client, len(env.TrustedClients))}
	seenFingerprint := make(map[string]struct{}, len(env.TrustedClients))
	for i, c := range env.TrustedClients {
		if reason, ok := validateClient(c); !ok {
			return nil, declErr(i, reason)
		}
		if _, dup := set.byName[c.Name]; dup {
			return nil, declErr(i, ReasonNameDeclaredTwice)
		}
		if _, dup := seenFingerprint[c.Fingerprint]; dup {
			return nil, declErr(i, ReasonFingerprintDeclaredTwice)
		}
		set.byName[c.Name] = cloneClient(c)
		seenFingerprint[c.Fingerprint] = struct{}{}
	}
	return set, nil
}

// classifyDecodeError reduces a JSON decoder error to a bounded reason. The error
// itself is never returned, wrapped or logged: encoding/json quotes the offending
// key or value, and a declaration's keys and values are deployment configuration.
func classifyDecodeError(err error) Reason {
	if strings.Contains(err.Error(), "unknown field") {
		return ReasonUnknownField
	}
	return ReasonInvalidJSON
}

// validateClient applies every field rule and reports the BOUNDED reason the record
// was rejected for. It REJECTS a malformed entry rather than repairing it: a
// partially-repaired declaration would not be the reviewed one, and the whole point
// of the seal is that what it states is what was approved.
//
// It returns a reason CODE rather than a message, and deliberately never sees a
// caller that could interpolate a value: the record index plus the rule is the whole
// diagnostic, because every other field of a record is deployment configuration.
func validateClient(c Client) (Reason, bool) {
	switch {
	case strings.TrimSpace(c.Name) == "":
		return ReasonNameEmpty, false
	case c.Name != strings.TrimSpace(c.Name):
		return ReasonNameNotTrimmed, false
	case len(c.Name) > maxNameLen:
		return ReasonNameTooLong, false
	case !fingerprintForm.MatchString(c.Fingerprint):
		return ReasonFingerprintNotOpaque, false
	case strings.TrimSpace(c.Provider) == "":
		return ReasonProviderEmpty, false
	case len(c.Options) == 0:
		return ReasonOptionsEmpty, false
	case len(c.Options) > bamlutils.MaxTrustedConfigOptions:
		return ReasonTooManyOptions, false
	}
	for k, v := range c.Options {
		if strings.TrimSpace(k) == "" {
			return ReasonOptionKeyEmpty, false
		}
		if v == "" {
			// The KEY is not named: an option key is free text a declaration chose,
			// so it can carry a URL, a model name or a credential just as a value
			// can. The record index is the handle.
			return ReasonOptionValueEmpty, false
		}
	}
	return "", true
}

func cloneClient(c Client) Client {
	out := c
	out.Options = make(map[string]string, len(c.Options))
	for k, v := range c.Options {
		out.Options[k] = v
	}
	return out
}

// Len reports how many configuration classes the deployment approved.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.byName)
}

// Seal applies the declaration to one request's registry, in place.
//
// For each client the request supplied:
//
//   - no declaration with that name -> left EXACTLY as it arrived, unsealed;
//   - the request supplied a provider, any option, or a retry policy -> left
//     EXACTLY as it arrived, unsealed. The caller tried to DEFINE the
//     configuration rather than name it, and a definition the caller wrote is
//     never an identity, even when its bytes match;
//   - otherwise -> the declared provider and options are INSTALLED and the client
//     is SEALED with the declared fingerprint plus a canonical digest of the
//     configuration as installed.
//
// It never removes a client, never renames one, and never touches a client it did
// not seal — so a deployment that declared nothing changes no request at all. Any
// pre-existing seal is cleared first: only this pass may create one.
//
// A nil *Set, a nil registry, and an empty declaration are all no-ops.
func (s *Set) Seal(reg *bamlutils.ClientRegistry) {
	if s.Len() == 0 || reg == nil {
		return
	}
	for _, cp := range reg.Clients {
		if cp == nil {
			continue
		}
		// Defensive: a seal can only be created here, so anything already carrying
		// one arrived by a route that must not confer identity.
		cp.ClearTrustedConfigSeal()

		declared, ok := s.byName[cp.Name]
		if !ok {
			continue
		}
		if cp.IsProviderPresent() || len(cp.Options) > 0 || cp.RetryPolicy != nil {
			// The request DEFINED part of this configuration. Leave it entirely
			// alone — both the values, so behaviour is unchanged, and the seal, so
			// it carries no identity.
			continue
		}
		cp.Provider = declared.Provider
		cp.ProviderSet = true
		cp.Options = make(map[string]any, len(declared.Options))
		for k, v := range declared.Options {
			cp.Options[k] = v
		}
		digest, err := bamlutils.TrustedConfigDigest(cp, declared.Provider)
		if err != nil {
			// The declaration cannot be canonicalized. The values are still
			// installed (the deployment asked for them), but no identity is
			// conferred: an unsealed client simply has none.
			continue
		}
		cp.SealTrustedConfig(declared.Fingerprint, digest)
	}
}
