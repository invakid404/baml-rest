package testutil

// Pure, ungated unit tests for the BAML-free / nanollm-free comparator. They
// import neither runtime and need no CGO, so they document and pin the
// normalization, redaction, semantic-filtering, and mutation-sensitivity
// behavior independently of the gated legs. They are NOT P5 parity evidence
// (the scope requires the gated legs for that) — they only prove the shared
// comparator itself is correct and fail closed.

import (
	"strings"
	"testing"
)

func TestNormalizeHeadersLowercasesNames(t *testing.T) {
	m, err := NormalizeHeaders([][2]string{
		{"Content-Type", "application/json"},
		{"Authorization", "Bearer x"},
	})
	if err != nil {
		t.Fatalf("NormalizeHeaders: %v", err)
	}
	if m["content-type"] != "application/json" || m["authorization"] != "Bearer x" {
		t.Fatalf("names not folded to lowercase: %v", m)
	}
}

func TestNormalizeHeadersRejectsDuplicateNames(t *testing.T) {
	// Same name in different casing must be rejected (case-insensitive uniqueness).
	if _, err := NormalizeHeaders([][2]string{
		{"Authorization", "Bearer a"},
		{"authorization", "Bearer b"},
	}); err == nil {
		t.Fatal("duplicate header name (case-insensitive) must be rejected")
	}
}

func TestPairsFromMultiMapRejectsMultiValue(t *testing.T) {
	// A multi-value header flattens to repeated pairs and must be rejected.
	pairs := PairsFromMultiMap(map[string][]string{"X-Multi": {"a", "b"}})
	if _, err := NormalizeHeaders(pairs); err == nil {
		t.Fatal("multi-value header must be rejected")
	}
	// A single-value header is fine.
	if _, err := NormalizeHeaders(PairsFromMultiMap(map[string][]string{"X-One": {"a"}})); err != nil {
		t.Fatalf("single-value header must normalize: %v", err)
	}
}

func TestSplitSemanticDropsBamlTransportHeader(t *testing.T) {
	sem, transport := SplitSemantic(map[string]string{
		"content-type":      "application/json",
		"authorization":     "Bearer x",
		"baml-original-url": "https://static-oracle.invalid/v1",
	})
	if _, ok := sem["baml-original-url"]; ok {
		t.Error("baml-original-url must not be in the semantic set")
	}
	if _, ok := transport["baml-original-url"]; !ok {
		t.Error("baml-original-url must be partitioned into the transport set")
	}
	if len(sem) != 2 {
		t.Errorf("semantic set = %v, want content-type + authorization only", sem)
	}
}

func TestDiffEqualSnapshots(t *testing.T) {
	// BAML carries the extra transport header; nanollm does not. They must still
	// be considered equal (the transport header is declined, not compared).
	a, err := NewSnapshot("POST", "https://static-oracle.invalid/v1/chat/completions",
		[][2]string{
			{"content-type", "application/json"},
			{"authorization", "Bearer fake-static-oracle-key"},
			{"baml-original-url", "https://static-oracle.invalid/v1"},
		}, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("NewSnapshot(baml): %v", err)
	}
	b, err := NewSnapshot("POST", "https://static-oracle.invalid/v1/chat/completions",
		[][2]string{
			{"Content-Type", "application/json"},
			{"Authorization", "Bearer fake-static-oracle-key"},
		}, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("NewSnapshot(nanollm): %v", err)
	}
	if d := Diff(a, b); len(d) != 0 {
		t.Fatalf("equal snapshots reported diffs: %v", d)
	}
}

// TestDiffBamlOriginalURLExemptionIsOracleOnly pins that the baml-original-url
// transport-header exemption is ONE-SIDED (oracle/`a` only). If nanollm (`b`)
// ever emitted baml-original-url it must FAIL parity, never be silently exempted.
func TestDiffBamlOriginalURLExemptionIsOracleOnly(t *testing.T) {
	const url = "https://static-oracle.invalid/v1/chat/completions"
	body := []byte(`{"model":"m"}`)
	semantic := [][2]string{
		{"content-type", "application/json"},
		{"authorization", "Bearer fake-static-oracle-key"},
	}
	withTransport := append(append([][2]string(nil), semantic...),
		[2]string{"baml-original-url", "https://static-oracle.invalid/v1"})

	mk := func(pairs [][2]string) Snapshot {
		s, err := NewSnapshot("POST", url, pairs, body)
		if err != nil {
			t.Fatalf("NewSnapshot: %v", err)
		}
		return s
	}

	// Oracle side (a) carries baml-original-url, nanollm side (b) does not: the
	// exemption drops it from a, so parity holds (the real-fixture case).
	if d := Diff(mk(withTransport), mk(semantic)); len(d) != 0 {
		t.Fatalf("baml-original-url on the ORACLE side must be exempted (no diff), got: %v", d)
	}

	// nanollm side (b) carries baml-original-url, oracle side (a) does not: the
	// exemption is NOT applied to b, so parity must FAIL (present on nanollm only).
	d := Diff(mk(semantic), mk(withTransport))
	if len(d) == 0 {
		t.Fatal("baml-original-url on the NANOLLM side must FAIL parity — the exemption must be oracle-only")
	}
	joined := strings.Join(d, "\n")
	if !strings.Contains(joined, "baml-original-url") || !strings.Contains(joined, "present on nanollm only") {
		t.Fatalf("expected a 'present on nanollm only' diff for baml-original-url, got: %v", d)
	}
}

func TestDiffDetectsMethodURLBodyDifferences(t *testing.T) {
	base := func() (Snapshot, Snapshot) {
		a, _ := NewSnapshot("POST", "u",
			[][2]string{{"content-type", "application/json"}, {"authorization", "Bearer k"}}, []byte("body"))
		b, _ := NewSnapshot("POST", "u",
			[][2]string{{"content-type", "application/json"}, {"authorization", "Bearer k"}}, []byte("body"))
		return a, b
	}

	// Method mutation.
	a, b := base()
	b.Method = "GET"
	if d := Diff(a, b); len(d) == 0 || !containsPrefix(d, "method:") {
		t.Errorf("method mutation not detected: %v", d)
	}

	// URL mutation.
	a, b = base()
	b.URL = "u2"
	if d := Diff(a, b); len(d) == 0 || !containsPrefix(d, "url:") {
		t.Errorf("url mutation not detected: %v", d)
	}

	// One-byte body mutation.
	a, b = base()
	mutated := append([]byte(nil), b.Body...)
	mutated[0] ^= 0x20
	b.Body = mutated
	if d := Diff(a, b); len(d) == 0 || !containsPrefix(d, "body:") {
		t.Errorf("body mutation not detected: %v", d)
	}
}

func TestDiffAuthorizationMutationRedacted(t *testing.T) {
	a, _ := NewSnapshot("POST", "u",
		[][2]string{{"authorization", "Bearer real-key"}}, []byte("b"))
	b, _ := NewSnapshot("POST", "u",
		[][2]string{{"authorization", "Bearer other-key"}}, []byte("b"))
	d := Diff(a, b)
	if len(d) == 0 {
		t.Fatal("authorization value mutation not detected")
	}
	joined := strings.Join(d, "\n")
	if strings.Contains(joined, "real-key") || strings.Contains(joined, "other-key") {
		t.Fatalf("authorization value leaked into diagnostics: %q", joined)
	}
	if !strings.Contains(joined, "<redacted>") {
		t.Fatalf("authorization diff must be redacted: %q", joined)
	}
}

func TestRedactValue(t *testing.T) {
	if got := RedactValue("Content-Type", "application/json"); got != "application/json" {
		t.Errorf("content-type must not be redacted, got %q", got)
	}
	if got := RedactValue("Authorization", "Bearer secret"); got != "<redacted>" {
		t.Errorf("authorization must be redacted, got %q", got)
	}
}

func TestAuthorization(t *testing.T) {
	v, ok := Authorization(map[string]string{"authorization": "Bearer k"})
	if !ok || v != "Bearer k" {
		t.Fatalf("Authorization = %q,%v", v, ok)
	}
	if _, ok := Authorization(map[string]string{"content-type": "application/json"}); ok {
		t.Fatal("Authorization must report absence")
	}
}

func containsPrefix(diffs []string, prefix string) bool {
	for _, d := range diffs {
		if strings.HasPrefix(d, prefix) {
			return true
		}
	}
	return false
}
