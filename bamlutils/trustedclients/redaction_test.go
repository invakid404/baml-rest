package trustedclients

// De-BAML serving cutover S3a — the DECLARATION-ERROR REDACTION proof.
//
// A trusted-client declaration is deployment configuration: it holds the real model,
// the real endpoint and the real credential. So the REJECTION path is as much a
// non-observability surface as the metric labels are — the boot paths log the
// returned error verbatim, and a rejected declaration is exactly the situation in
// which someone is reading logs.
//
// This drives HOSTILE values — a URL, a model name, an API key, a prompt fragment —
// through every rejection path there is, and requires that none of them, and none of
// the raw declaration bytes, survive into the error. The reason set is enumerated
// from AllReasons so a new rejection path that forgets to redact has nowhere to hide:
// the coverage check below fails until it is driven here too.

import (
	"errors"
	"strings"
	"testing"
)

// hostileValues are the things a declaration could contain that must never appear in
// an error. Each is used as a NAME, a FINGERPRINT, an OPTION KEY and an OPTION VALUE
// across the cases below, because every one of those is free text a declaration chose.
func hostileValues() []string {
	return []string{
		"https://secrets.example/v1?token=abc",
		"gpt-4o-acme-tuned-2026",
		"sk-live-51H8xQhostile",
		"AKIAIOSFODNN7EXAMPLE",
		"the-user-prompt-text",
	}
}

// declarationRejections are the malformed declarations, one per bounded reason. Each
// carries a hostile value wherever the shape allows one.
func declarationRejections(hostile string) []struct {
	reason Reason
	spec   string
} {
	q := func(s string) string { return `"` + s + `"` }
	return []struct {
		reason Reason
		spec   string
	}{
		{ReasonInvalidJSON, `{"trusted_clients":[{"name":` + q(hostile) + `,}`},
		{ReasonUnknownField, `{"trusted_clients":[{"name":"A","fingerprint":"cfg001","provider":"openai","options":{"model":"m"},` + q(hostile) + `:1}]}`},
		{ReasonTrailingContent, `{"trusted_clients":[]} {"leaked":` + q(hostile) + `}`},
		{ReasonTooManyClients, tooManyClientsSpec(hostile)},
		{ReasonNameEmpty, `{"trusted_clients":[{"name":"","fingerprint":"cfg001","provider":` + q(hostile) + `,"options":{"model":` + q(hostile) + `}}]}`},
		{ReasonNameNotTrimmed, `{"trusted_clients":[{"name":" ` + hostile + ` ","fingerprint":"cfg001","provider":"openai","options":{"model":"m"}}]}`},
		{ReasonNameTooLong, `{"trusted_clients":[{"name":"` + strings.Repeat("x", maxNameLen) + hostile + `","fingerprint":"cfg001","provider":"openai","options":{"model":"m"}}]}`},
		{ReasonFingerprintNotOpaque, `{"trusted_clients":[{"name":` + q(hostile) + `,"fingerprint":` + q(hostile) + `,"provider":"openai","options":{"model":` + q(hostile) + `}}]}`},
		{ReasonProviderEmpty, `{"trusted_clients":[{"name":` + q(hostile) + `,"fingerprint":"cfg001","provider":"","options":{"model":` + q(hostile) + `}}]}`},
		{ReasonOptionsEmpty, `{"trusted_clients":[{"name":` + q(hostile) + `,"fingerprint":"cfg001","provider":"openai","options":{}}]}`},
		{ReasonTooManyOptions, tooManyOptionsSpec(hostile)},
		{ReasonOptionKeyEmpty, `{"trusted_clients":[{"name":` + q(hostile) + `,"fingerprint":"cfg001","provider":"openai","options":{"":` + q(hostile) + `}}]}`},
		{ReasonOptionValueEmpty, `{"trusted_clients":[{"name":` + q(hostile) + `,"fingerprint":"cfg001","provider":"openai","options":{` + q(hostile) + `:""}}]}`},
		{ReasonNameDeclaredTwice, `{"trusted_clients":[` + oneClient(hostile, "cfg001", hostile) + `,` + oneClient(hostile, "cfg002", hostile) + `]}`},
		{ReasonFingerprintDeclaredTwice, `{"trusted_clients":[` + oneClient(hostile+"-a", "cfg001", hostile) + `,` + oneClient(hostile+"-b", "cfg001", hostile) + `]}`},
	}
}

func oneClient(name, fingerprint, model string) string {
	return `{"name":"` + name + `","fingerprint":"` + fingerprint + `","provider":"openai","options":{"model":"` + model + `"}}`
}

func tooManyClientsSpec(hostile string) string {
	parts := make([]string, 0, MaxClients+1)
	for i := 0; i <= MaxClients; i++ {
		parts = append(parts, oneClient(hostile+"-"+string(rune('a'+i%26))+"-"+string(rune('a'+i/26)), "cfg0"+string(rune('0'+i/10))+string(rune('0'+i%10)), hostile))
	}
	return `{"trusted_clients":[` + strings.Join(parts, ",") + `]}`
}

func tooManyOptionsSpec(hostile string) string {
	opts := make([]string, 0, 32)
	for i := 0; i < 32; i++ {
		opts = append(opts, `"`+hostile+"-"+string(rune('a'+i))+`":"`+hostile+`"`)
	}
	return `{"trusted_clients":[{"name":"A","fingerprint":"cfg001","provider":"openai","options":{` + strings.Join(opts, ",") + `}}]}`
}

// TestDeclarationErrorsAreValueFree is the redaction proof.
func TestDeclarationErrorsAreValueFree(t *testing.T) {
	covered := map[Reason]bool{}
	for _, hostile := range hostileValues() {
		for _, tc := range declarationRejections(hostile) {
			t.Run(string(tc.reason)+"/"+hostile[:8], func(t *testing.T) {
				set, err := Parse(tc.spec)
				if err == nil {
					t.Fatalf("declaration was ACCEPTED; this case must reject so its error can be inspected")
				}
				if set != nil {
					t.Errorf("a rejected declaration returned a usable set")
				}
				var de *DeclarationError
				if !errors.As(err, &de) {
					t.Fatalf("error is %T, not a bounded *DeclarationError: %v", err, err)
				}
				if de.Reason != tc.reason {
					t.Errorf("reason = %q, want %q", de.Reason, tc.reason)
				}
				covered[de.Reason] = true

				msg := err.Error()
				// The hostile value, in any of the four positions it was planted in.
				if strings.Contains(msg, hostile) {
					t.Errorf("the error carries the declared value %q: %s", hostile, msg)
				}
				// And no fragment of the raw declaration either — a decoder error that
				// quoted a key or a token would show up here. The bounded reason token
				// is removed first: reason CODES legitimately contain schema words
				// (options_empty, provider_empty), and they name the rule that was
				// broken, not anything the declaration said.
				scanned := strings.ReplaceAll(msg, string(de.Reason), "")
				for _, fragment := range []string{"trusted_clients", "options", "provider", "fingerprint", "name", "{", "}", "\""} {
					if strings.Contains(scanned, fragment) {
						t.Errorf("the error carries raw declaration text %q: %s", fragment, msg)
					}
				}
				// It stays useful: the variable an operator sets, and the record index.
				if !strings.Contains(msg, EnvVar) {
					t.Errorf("the error does not name %s, so an operator cannot tell what to fix: %s", EnvVar, msg)
				}
				if !strings.Contains(msg, string(tc.reason)) {
					t.Errorf("the error does not carry its bounded reason: %s", msg)
				}
			})
		}
	}
	// NON-VACUITY + COVERAGE: every declared reason must have been produced by a real
	// rejection above. A new rejection path that forgets to redact cannot be added
	// without appearing here.
	for _, r := range AllReasons() {
		if !covered[r] {
			t.Errorf("reason %q is declared but no rejection case produces it; the redaction proof does not cover it", r)
		}
	}
}

// TestDeclarationErrorRecordIndexIsTheOperatorHandle pins what redaction leaves
// behind: enough to act on, and nothing to leak. The index must point at the record
// that actually broke the rule.
func TestDeclarationErrorRecordIndexIsTheOperatorHandle(t *testing.T) {
	good := oneClient("Good", "cfg001", "m")
	bad := oneClient("Bad", "not-opaque", "m")
	_, err := Parse(`{"trusted_clients":[` + good + `,` + bad + `]}`)
	var de *DeclarationError
	if !errors.As(err, &de) {
		t.Fatalf("error is %T, want *DeclarationError: %v", err, err)
	}
	if de.Record != 1 {
		t.Errorf("Record = %d, want 1 — the index must point at the offending record", de.Record)
	}
	if de.Reason != ReasonFingerprintNotOpaque {
		t.Errorf("Reason = %q, want %q", de.Reason, ReasonFingerprintNotOpaque)
	}
	if !strings.Contains(err.Error(), "record 1") {
		t.Errorf("the message does not carry the record index: %s", err.Error())
	}

	// A document-level failure has no record to point at, and says so rather than
	// implying record 0.
	_, err = Parse(`{`)
	if !errors.As(err, &de) {
		t.Fatalf("error is %T, want *DeclarationError", err)
	}
	if de.Record >= 0 {
		t.Errorf("a document-level failure reported record %d, want a negative sentinel", de.Record)
	}
	if strings.Contains(err.Error(), "record") {
		t.Errorf("a document-level failure names a record: %s", err.Error())
	}
}
