package codegen

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// TestResolveOptions_SupportsWithClient_TopLevelOnly pins that an
// Options whose top-level SupportsWithClient is true but whose
// Introspection.SupportsWithClient mirror is false still drives the
// generator's WithClient emission path. resolveOptions OR-folds both
// fields so the codegen dispatch (which reads
// resolved.SupportsWithClient) and the fail-fast invariant (which
// reads g.intro.{Request,StreamRequest} alongside g.supportsWithClient)
// both see the same canonical value.
func TestResolveOptions_SupportsWithClient_TopLevelOnly(t *testing.T) {
	opts := Options{
		SelfPkg:            "github.com/example/test",
		SupportsWithClient: true,
		Introspection: Introspection{
			SupportsWithClient: false,
			SyncMethods:        map[string][]string{}, // non-nil so RootIntrospection() isn't substituted
			MediaParams:        map[string]map[string]bamlutils.MediaKind{},
		},
	}
	resolved := resolveOptions(opts)
	if !resolved.SupportsWithClient {
		t.Errorf("top-level SupportsWithClient must survive: got %v", resolved.SupportsWithClient)
	}
	if !resolved.Introspection.SupportsWithClient {
		t.Errorf("Introspection.SupportsWithClient must be propagated from the top level: got %v", resolved.Introspection.SupportsWithClient)
	}
}

// TestResolveOptions_SupportsWithClient_IntrospectionOnly pins the
// symmetric case: only Introspection.SupportsWithClient is set (the
// AST-detected mirror cmd/introspect emits), and resolveOptions must
// propagate it back to the top-level field so g.supportsWithClient
// turns on at the generator. Without this fold the codegen dispatch
// would silently fall back to legacy-only emission because it reads
// only resolved.SupportsWithClient.
func TestResolveOptions_SupportsWithClient_IntrospectionOnly(t *testing.T) {
	opts := Options{
		SelfPkg: "github.com/example/test",
		Introspection: Introspection{
			SupportsWithClient: true,
			SyncMethods:        map[string][]string{},
			MediaParams:        map[string]map[string]bamlutils.MediaKind{},
		},
	}
	resolved := resolveOptions(opts)
	if !resolved.SupportsWithClient {
		t.Errorf("top-level SupportsWithClient must be propagated from Introspection: got %v", resolved.SupportsWithClient)
	}
	if !resolved.Introspection.SupportsWithClient {
		t.Errorf("Introspection.SupportsWithClient must survive: got %v", resolved.Introspection.SupportsWithClient)
	}
}

// TestResolveOptions_SupportsWithClient_Both pins the no-op case: both
// sources already true, both stay true.
func TestResolveOptions_SupportsWithClient_Both(t *testing.T) {
	opts := Options{
		SelfPkg:            "github.com/example/test",
		SupportsWithClient: true,
		Introspection: Introspection{
			SupportsWithClient: true,
			SyncMethods:        map[string][]string{},
			MediaParams:        map[string]map[string]bamlutils.MediaKind{},
		},
	}
	resolved := resolveOptions(opts)
	if !resolved.SupportsWithClient || !resolved.Introspection.SupportsWithClient {
		t.Errorf("both fields must stay true; got top=%v intro=%v", resolved.SupportsWithClient, resolved.Introspection.SupportsWithClient)
	}
}

// TestResolveOptions_SupportsWithClient_Neither pins the negative case:
// neither source set, the fold must NOT flip either field on. Auto-
// deriving SupportsWithClient from the Request/StreamRequest singletons
// would let codegen emit WithClient calls against a runtime that lacks
// the symbol, so the fold is intentionally strict OR over the two
// SupportsWithClient sources and nothing else.
func TestResolveOptions_SupportsWithClient_Neither(t *testing.T) {
	opts := Options{
		SelfPkg: "github.com/example/test",
		Introspection: Introspection{
			SupportsWithClient: false,
			SyncMethods:        map[string][]string{},
			MediaParams:        map[string]map[string]bamlutils.MediaKind{},
		},
	}
	resolved := resolveOptions(opts)
	if resolved.SupportsWithClient || resolved.Introspection.SupportsWithClient {
		t.Errorf("neither field set; resolveOptions must not flip them on; got top=%v intro=%v", resolved.SupportsWithClient, resolved.Introspection.SupportsWithClient)
	}
}

// TestPkgPathMatchesBAML_NonDefaultBamlPkg pins the dynclient-shaped
// case: a caller supplies a non-default Packages.BamlPkg (e.g. a forked
// patched-BAML module under the dynclient internal tree), and a
// reflected media type's PkgPath sits exactly at or under that custom
// path. The legacy substring matcher ("boundaryml/baml" /
// "baml_client") would silently return false for both, dropping media
// handling for every Image/Audio/PDF/Video field. The new comparator
// must match the custom prefix.
func TestPkgPathMatchesBAML_NonDefaultBamlPkg(t *testing.T) {
	pkgs := PackageConfig{
		BamlPkg:            "example.com/forked/baml/engine/language_client_go/pkg",
		GeneratedClientPkg: "example.com/app/dynclient/internal/generated/baml_client",
	}

	cases := []struct {
		name    string
		pkgPath string
		want    bool
	}{
		{"exact BamlPkg", "example.com/forked/baml/engine/language_client_go/pkg", true},
		{"BamlPkg subpackage", "example.com/forked/baml/engine/language_client_go/pkg/sub", true},
		{"exact GeneratedClientPkg", "example.com/app/dynclient/internal/generated/baml_client", true},
		{"GeneratedClientPkg subpackage", "example.com/app/dynclient/internal/generated/baml_client/types", true},
		{"unrelated upstream BAML (substring would match)", "github.com/boundaryml/baml/engine/language_client_go/pkg", false},
		{"unrelated baml_client substring would match", "example.com/other/baml_client", false},
		{"empty path", "", false},
		{"sibling sharing prefix but no slash", "example.com/forked/baml/engine/language_client_go/pkgx", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pkgPathMatchesBAML(c.pkgPath, pkgs)
			if got != c.want {
				t.Errorf("pkgPathMatchesBAML(%q, %#v) = %v, want %v", c.pkgPath, pkgs, got, c.want)
			}
		})
	}
}

// TestPkgPathMatchesBAML_DefaultPackageConfig confirms that the
// production server's defaults keep recognising upstream BAML and the
// root baml_client. This is the byte-identical-output side of the
// contract: a stricter prefix matcher would silently change media
// detection behaviour for the existing root build.
func TestPkgPathMatchesBAML_DefaultPackageConfig(t *testing.T) {
	pkgs := DefaultPackageConfig()
	cases := []struct {
		name    string
		pkgPath string
		want    bool
	}{
		{"upstream BAML runtime", "github.com/boundaryml/baml/engine/language_client_go/pkg", true},
		{"root generated client", "github.com/invakid404/baml-rest/baml_client", true},
		{"root generated client subpackage", "github.com/invakid404/baml-rest/baml_client/types", true},
		{"unrelated", "example.com/other", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pkgPathMatchesBAML(c.pkgPath, pkgs)
			if got != c.want {
				t.Errorf("pkgPathMatchesBAML(%q, default) = %v, want %v", c.pkgPath, got, c.want)
			}
		})
	}
}
