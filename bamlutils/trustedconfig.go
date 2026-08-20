package bamlutils

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"
)

// De-BAML serving cutover S3a — the TRUSTED-CONFIGURATION SEAL.
//
// # The provenance problem this exists for
//
// The dynamic `/call` surface takes its `client_registry` from the PUBLIC REQUEST
// BODY: the caller names the clients, their provider, and every option including
// `model`, `base_url` and `api_key`. Everything derived from that registry —
// which leaf BAML selects, what its effective target is, what its options are — is
// therefore CALLER-SUPPLIED, however self-consistent it looks.
//
// That is fine for BAML transport, which is what the caller asked for. It is NOT a
// basis for a configuration IDENTITY: an identity says "this request is running the
// deployment's approved configuration class", and a value the caller sent cannot
// establish that. Matching the approved shape byte-for-byte does not either — a
// caller who supplies the approved client name, provider, model and base_url with
// THEIR OWN credential is running a different configuration under a matching mask.
//
// So identity needs a fact the request cannot manufacture. That fact is this seal.
//
// # What the seal means, exactly
//
// A sealed client is one the DEPLOYMENT configured: the request did no more than
// NAME it, and the worker's config-load path installed the provider and every
// option from the deployment's own approved-configuration declaration. The seal
// carries the opaque configuration fingerprint the deployment assigned, plus a
// canonical digest of the effective configuration as installed, so a later
// consumer can prove the configuration it is looking at is still the one that was
// sealed rather than something a subsequent pass mutated.
//
// # Why an unexported field is the boundary
//
// A request is JSON. An unexported field is unreachable from JSON — not by
// convention but structurally: sonic and encoding/json only touch exported fields,
// [ClientProperty.UnmarshalJSON] rebuilds the value through an alias whose seal is
// the zero value (so decoding always CLEARS any seal), and
// [ClientProperty.MarshalJSON] emits only exported fields (so a seal never leaves
// the process either). There is no wire representation to forge.
//
// [ClientProperty.SealTrustedConfig] is exported because the sealing pass lives in
// another package (the worker's config load), and that is the same trust class as
// every other server-side config-load API: code running inside the deployment,
// applying the deployment's own declarations. It is NOT reachable by an HTTP
// caller, which is the boundary that matters here.

// trustedConfigSeal is the sealed statement itself. It is a pointer field on
// [ClientProperty] so the zero value is unambiguously "not sealed" and a struct
// copy cannot half-carry one.
type trustedConfigSeal struct {
	// fingerprint is the opaque configuration bucket the deployment assigned to
	// this approved configuration class. It is deliberately an opaque bounded
	// token, never a name/URL/model/credential; the consumer re-validates it
	// against its own declared vocabulary before it can mean anything.
	fingerprint string
	// digest is the canonical selector digest of the effective configuration AS
	// INSTALLED by the sealing pass. A consumer recomputes it from the client it
	// is actually looking at and refuses the seal on any difference, so a pass
	// that mutated a sealed client after the seal was applied cannot inherit its
	// identity.
	digest string
}

// SealTrustedConfig marks this client as the deployment's approved configuration
// class `fingerprint`, whose effective configuration digests to `digest`.
//
// TRUST CONTRACT — call this ONLY from a config-load path applying the
// deployment's own declaration, and ONLY for a client whose provider and options
// that path installed itself. Sealing a client whose values came from the request
// would defeat the entire boundary this file exists to draw. An empty fingerprint
// or digest clears the seal rather than storing a meaningless one.
func (c *ClientProperty) SealTrustedConfig(fingerprint, digest string) {
	if c == nil {
		return
	}
	if fingerprint == "" || digest == "" {
		c.seal = nil
		return
	}
	c.seal = &trustedConfigSeal{fingerprint: fingerprint, digest: digest}
}

// TrustedConfigSeal reports the seal this client carries. sealed is false — and the
// other two results are empty — for every client the deployment did not configure,
// which is every client on a deployment that declared nothing.
func (c *ClientProperty) TrustedConfigSeal() (fingerprint, digest string, sealed bool) {
	if c == nil || c.seal == nil {
		return "", "", false
	}
	return c.seal.fingerprint, c.seal.digest, true
}

// ClearTrustedConfigSeal removes any seal. It exists so a pass that MUTATES a
// client can drop an identity that no longer describes it, rather than leaving a
// stale one behind for the digest check to catch later.
func (c *ClientProperty) ClearTrustedConfigSeal() {
	if c != nil {
		c.seal = nil
	}
}

// trustedConfigDigestDomain is the domain separator every canonical selector
// digest starts with, so a digest computed for this purpose cannot collide with
// one computed for another purpose over the same bytes.
const trustedConfigDigestDomain = "baml-rest/debaml/config-selector/v1"

// trustedConfigCredentialSentinel is what a credential-valued client option
// contributes to the canonical encoding IN PLACE of its value. The configuration
// class is the same class after a key rotation, and a digest that moved when a
// credential rotated would invalidate a deployment's own seal for no reason.
// Presence still matters — a configuration carrying no api_key at all is a
// different configuration — so the sentinel is written, not skipped.
const trustedConfigCredentialSentinel = "\x00credential"

// MaxTrustedConfigOptions caps how many client options the canonical encoding will
// digest. The strict OpenAI anchor accepts three; anything remotely near this cap
// is an unproven shape a native predicate declines anyway.
const MaxTrustedConfigOptions = 16

// trustedConfigCredentialKeys are the option keys whose VALUE is a credential.
// It is a closed reviewed set, not a heuristic.
func trustedConfigCredentialKeys() map[string]struct{} {
	return map[string]struct{}{"api_key": {}}
}

// TrustedConfigDigest returns the canonical selector digest of an effective client
// configuration under resolvedProvider: the value a sealing pass stores and a
// consumer recomputes.
//
// The encoding is length-prefixed throughout — every field is an 8-byte big-endian
// length followed by its bytes — so no combination of client name, provider
// spelling, option key or option value can be re-partitioned into a different
// configuration that digests the same. Nothing is normalized, trimmed or
// case-folded: `https://api.example/v1` and `https://api.example/v1/` build
// different request URLs, so they are different configurations.
//
// It is pure, and it returns an error rather than a best-effort digest for any
// configuration it cannot canonicalize (a non-literal option value, or more than
// [MaxTrustedConfigOptions] options). A caller must treat that error as "no
// identity", never as "digest what you can".
func TrustedConfigDigest(cp *ClientProperty, resolvedProvider string) (string, error) {
	if cp == nil {
		return "", fmt.Errorf("bamlutils: no client to digest")
	}
	if resolvedProvider == "" {
		return "", fmt.Errorf("bamlutils: no resolved provider to digest under")
	}
	if len(cp.Options) > MaxTrustedConfigOptions {
		return "", fmt.Errorf("bamlutils: client carries %d options, cap is %d", len(cp.Options), MaxTrustedConfigOptions)
	}
	keys := make([]string, 0, len(cp.Options))
	for k := range cp.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	writeTrustedConfigField(h, trustedConfigDigestDomain)
	writeTrustedConfigField(h, resolvedProvider)
	writeTrustedConfigField(h, cp.Name)
	writeTrustedConfigField(h, cp.Provider)
	writeTrustedConfigCount(h, len(keys))
	credential := trustedConfigCredentialKeys()
	for _, k := range keys {
		value, ok := cp.Options[k].(string)
		if !ok {
			// A non-literal option (a nested map, a list, a number, a null) has no
			// canonical string form here, and inventing one would let two different
			// configurations digest the same.
			return "", fmt.Errorf("bamlutils: client option %q is not a resolved literal string", k)
		}
		writeTrustedConfigField(h, k)
		if _, isCredential := credential[k]; isCredential {
			writeTrustedConfigField(h, trustedConfigCredentialSentinel)
			continue
		}
		writeTrustedConfigField(h, value)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeTrustedConfigField writes one length-prefixed field into the encoding.
func writeTrustedConfigField(h hash.Hash, s string) {
	writeTrustedConfigCount(h, len(s))
	_, _ = h.Write([]byte(s))
}

// writeTrustedConfigCount writes one 8-byte big-endian count into the encoding.
func writeTrustedConfigCount(h hash.Hash, n int) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(n))
	_, _ = h.Write(buf[:])
}
