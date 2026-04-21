package clientdefaults

import (
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// loadFromString is the test entry point. We avoid mutating the real env var
// so tests are safe under t.Parallel() and don't leak across packages.
func loadFromString(t *testing.T, raw string) (*Config, error) {
	t.Helper()
	return parse(raw)
}

func mustLoad(t *testing.T, raw string) *Config {
	t.Helper()
	c, err := loadFromString(t, raw)
	if err != nil {
		t.Fatalf("parse(%q) unexpected error: %v", raw, err)
	}
	if c == nil {
		t.Fatalf("parse(%q) returned nil config", raw)
	}
	return c
}

func newRegistry(clients ...*bamlutils.ClientProperty) *bamlutils.ClientRegistry {
	return &bamlutils.ClientRegistry{Clients: clients}
}

func TestLoad_EmptyEnv(t *testing.T) {
	cfg := mustLoad(t, "")
	reg := newRegistry(&bamlutils.ClientProperty{
		Name:     "A",
		Provider: "openai-generic",
		Options:  map[string]any{"model": "gpt-4"},
	})
	cfg.Apply(reg)
	if got := reg.Clients[0].Options["allowed_role_metadata"]; got != nil {
		t.Fatalf("expected no injection on empty config, got %v", got)
	}
	if len(reg.Clients[0].Options) != 1 {
		t.Fatalf("expected Options untouched, got %v", reg.Clients[0].Options)
	}
}

func TestLoad_InjectsDefaultWhenKeyAbsent(t *testing.T) {
	cfg := mustLoad(t, `{"client_defaults":{"options":{"allowed_role_metadata":["cache_control"]}}}`)
	client := &bamlutils.ClientProperty{
		Name:     "A",
		Provider: "openai-generic",
		Options:  map[string]any{"model": "gpt-4"},
	}
	cfg.Apply(newRegistry(client))

	got, ok := client.Options["allowed_role_metadata"].([]any)
	if !ok {
		t.Fatalf("expected []any, got %T: %v", client.Options["allowed_role_metadata"], client.Options["allowed_role_metadata"])
	}
	if len(got) != 1 || got[0] != "cache_control" {
		t.Fatalf("expected [cache_control], got %v", got)
	}
}

func TestLoad_CallerValueWins(t *testing.T) {
	cfg := mustLoad(t, `{"client_defaults":{"options":{"allowed_role_metadata":["cache_control"]}}}`)
	client := &bamlutils.ClientProperty{
		Options: map[string]any{
			"allowed_role_metadata": []any{"tenant_id"},
		},
	}
	cfg.Apply(newRegistry(client))

	got := client.Options["allowed_role_metadata"].([]any)
	if len(got) != 1 || got[0] != "tenant_id" {
		t.Fatalf("expected caller value [tenant_id] preserved, got %v", got)
	}
}

func TestLoad_CallerEmptyListOptOutWins(t *testing.T) {
	cfg := mustLoad(t, `{"client_defaults":{"options":{"allowed_role_metadata":["cache_control"]}}}`)
	client := &bamlutils.ClientProperty{
		Options: map[string]any{
			"allowed_role_metadata": []any{},
		},
	}
	cfg.Apply(newRegistry(client))

	got, ok := client.Options["allowed_role_metadata"].([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", client.Options["allowed_role_metadata"])
	}
	if len(got) != 0 {
		t.Fatalf("expected caller opt-out [] preserved, got %v", got)
	}
}

func TestLoad_CallerNoneOptOutWins(t *testing.T) {
	cfg := mustLoad(t, `{"client_defaults":{"options":{"allowed_role_metadata":["cache_control"]}}}`)
	client := &bamlutils.ClientProperty{
		Options: map[string]any{
			"allowed_role_metadata": "none",
		},
	}
	cfg.Apply(newRegistry(client))

	if got := client.Options["allowed_role_metadata"]; got != "none" {
		t.Fatalf("expected \"none\" preserved, got %v", got)
	}
}

func TestLoad_CallerNullWins(t *testing.T) {
	// Explicit null: caller wins at the merge layer. (BAML will reject this
	// downstream at LLM-client construction, per the package doc comment.)
	cfg := mustLoad(t, `{"client_defaults":{"options":{"allowed_role_metadata":["cache_control"]}}}`)
	client := &bamlutils.ClientProperty{
		Options: map[string]any{
			"allowed_role_metadata": nil,
		},
	}
	cfg.Apply(newRegistry(client))

	got, present := client.Options["allowed_role_metadata"]
	if !present {
		t.Fatalf("expected key to remain present with nil value")
	}
	if got != nil {
		t.Fatalf("expected nil preserved, got %v", got)
	}
}

func TestLoad_MultiClientInjection(t *testing.T) {
	cfg := mustLoad(t, `{"client_defaults":{"options":{"allowed_role_metadata":["cache_control"]}}}`)
	clients := []*bamlutils.ClientProperty{
		{Name: "A", Options: map[string]any{"model": "a"}},
		{Name: "B", Options: map[string]any{"model": "b"}},
		{Name: "C", Options: map[string]any{"model": "c"}},
	}
	cfg.Apply(&bamlutils.ClientRegistry{Clients: clients})

	for _, c := range clients {
		got, ok := c.Options["allowed_role_metadata"].([]any)
		if !ok || len(got) != 1 || got[0] != "cache_control" {
			t.Fatalf("client %s: expected [cache_control], got %v (type %T)",
				c.Name, c.Options["allowed_role_metadata"], c.Options["allowed_role_metadata"])
		}
	}
}

func TestLoad_NilOptionsMapGetsAllocated(t *testing.T) {
	cfg := mustLoad(t, `{"client_defaults":{"options":{"allowed_role_metadata":["cache_control"]}}}`)
	client := &bamlutils.ClientProperty{Name: "A"}
	if client.Options != nil {
		t.Fatalf("precondition: expected nil Options")
	}
	cfg.Apply(newRegistry(client))
	if client.Options == nil {
		t.Fatalf("expected Options to be lazily allocated")
	}
	if _, ok := client.Options["allowed_role_metadata"]; !ok {
		t.Fatalf("expected allowed_role_metadata to be set")
	}
}

func TestLoad_MalformedJSON(t *testing.T) {
	_, err := loadFromString(t, `{bad`)
	if err == nil {
		t.Fatalf("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "BAML_REST_CLIENT_DEFAULTS") {
		t.Fatalf("expected error to mention env var name, got %v", err)
	}
}

func TestLoad_UnknownOptionKey(t *testing.T) {
	_, err := loadFromString(t, `{"client_defaults":{"options":{"foo_bar":1}}}`)
	if err == nil {
		t.Fatalf("expected error for unknown option key")
	}
	if !strings.Contains(err.Error(), "foo_bar") {
		t.Fatalf("expected error to name unknown key foo_bar, got %v", err)
	}
}

func TestLoad_WrongValueType(t *testing.T) {
	_, err := loadFromString(t, `{"client_defaults":{"options":{"allowed_role_metadata":42}}}`)
	if err == nil {
		t.Fatalf("expected error for wrong value type")
	}
	if !strings.Contains(err.Error(), "allowed_role_metadata") {
		t.Fatalf("expected error to mention allowed_role_metadata, got %v", err)
	}
}

func TestLoad_UnknownEnvelopeKey(t *testing.T) {
	_, err := loadFromString(t, `{"client_default":{"options":{"allowed_role_metadata":["cache_control"]}}}`)
	if err == nil {
		t.Fatalf("expected error for unknown envelope key")
	}
}

func TestLoad_TrailingData(t *testing.T) {
	cases := []string{
		`{"client_defaults":{"options":{"allowed_role_metadata":["cache_control"]}}} trailing`,
		`{"client_defaults":{"options":{"allowed_role_metadata":["cache_control"]}}} }`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			_, err := loadFromString(t, raw)
			if err == nil {
				t.Fatalf("expected error for trailing data")
			}
			if !strings.Contains(err.Error(), "unexpected trailing data") {
				t.Fatalf("expected trailing-data error, got %v", err)
			}
		})
	}
}

func TestLoad_CloningGuarantee(t *testing.T) {
	// Two clients with no caller value should receive independent copies of
	// the default. Mutating one must not affect the other — and must not
	// affect the Config's internal parsed state.
	cfg := mustLoad(t, `{"client_defaults":{"options":{"allowed_role_metadata":["cache_control"]}}}`)
	client0 := &bamlutils.ClientProperty{Name: "A"}
	client1 := &bamlutils.ClientProperty{Name: "B"}
	cfg.Apply(&bamlutils.ClientRegistry{Clients: []*bamlutils.ClientProperty{client0, client1}})

	slice0 := client0.Options["allowed_role_metadata"].([]any)
	slice1 := client1.Options["allowed_role_metadata"].([]any)

	slice0 = append(slice0, "tenant_id")
	slice0[0] = "hijacked"
	client0.Options["allowed_role_metadata"] = slice0

	if got := client1.Options["allowed_role_metadata"].([]any); len(got) != 1 || got[0] != "cache_control" {
		t.Fatalf("client1 aliased client0's slice: %v", got)
	}
	_ = slice1

	// A fresh Apply on a third client must still emit the pristine default.
	client2 := &bamlutils.ClientProperty{Name: "C"}
	cfg.Apply(newRegistry(client2))
	if got := client2.Options["allowed_role_metadata"].([]any); len(got) != 1 || got[0] != "cache_control" {
		t.Fatalf("Config's parsed state was mutated: %v", got)
	}
}

func TestHasKey(t *testing.T) {
	cfg := mustLoad(t, `{"client_defaults":{"options":{"allowed_role_metadata":["cache_control"]}}}`)
	if !cfg.HasKey("allowed_role_metadata") {
		t.Fatalf("expected HasKey(allowed_role_metadata) = true")
	}
	if cfg.HasKey("headers") {
		t.Fatalf("expected HasKey(headers) = false")
	}

	empty := mustLoad(t, "")
	if empty.HasKey("allowed_role_metadata") {
		t.Fatalf("expected HasKey on empty config = false")
	}

	var nilCfg *Config
	if nilCfg.HasKey("anything") {
		t.Fatalf("HasKey on nil config must not panic and must return false")
	}
}

func TestApply_NilSafe(t *testing.T) {
	var nilCfg *Config
	nilCfg.Apply(newRegistry(&bamlutils.ClientProperty{}))

	cfg := mustLoad(t, `{"client_defaults":{"options":{"allowed_role_metadata":["cache_control"]}}}`)
	cfg.Apply(nil)
}
