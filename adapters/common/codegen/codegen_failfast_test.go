package codegen

import (
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/introspected"
)

// TestGenerate_FailsFastOnUnsupportedWithClientWithBuildRequest pins
// the SupportsWithClient invariant: when SupportsWithClient is false
// but the introspected Request / StreamRequest singletons are non-nil,
// codegen would emit BuildRequest paths that compile but cannot honor
// the per-attempt clientOverride that fallback / round-robin /
// dynamic-primary semantics depend on. The fail-fast guard panics
// before any code is emitted so a misconfigured Options + introspect
// pair surfaces at generator time, not in production.
//
// This is a configuration-invariant test: the shipped adapter matrix
// never trips it (v0.204/v0.215 expose neither the BR API nor
// WithClient; v0.219 sets SupportsWithClient=true). The guard catches
// custom BAML Go libraries, future partial API shapes, and direct
// GenerateWithOptions misuse.
func TestGenerate_FailsFastOnUnsupportedWithClientWithBuildRequest(t *testing.T) {
	cases := []struct {
		name string
		set  func()
		// wantSubstr is a per-case token unique to the singleton state
		// the case sets up. A substring match on "Request" alone
		// would appear in every panic message regardless of which
		// singleton was set — masking a wrong-singleton panic. The
		// panic message includes "Request=true/false,
		// StreamRequest=true/false"; each case asserts the exact pair
		// its setup produced.
		wantSubstr string
	}{
		{
			name:       "introspected.Request non-nil (call BuildRequest)",
			set:        func() { introspected.Request = struct{}{} },
			wantSubstr: "Request=true, StreamRequest=false",
		},
		{
			name:       "introspected.StreamRequest non-nil (stream BuildRequest)",
			set:        func() { introspected.StreamRequest = struct{}{} },
			wantSubstr: "Request=false, StreamRequest=true",
		},
		{
			name: "both non-nil",
			set: func() {
				introspected.Request = struct{}{}
				introspected.StreamRequest = struct{}{}
			},
			wantSubstr: "Request=true, StreamRequest=true",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Save + restore the introspected singletons. This package
			// has global state that other codegen tests may rely on.
			savedRequest := introspected.Request
			savedStreamRequest := introspected.StreamRequest
			t.Cleanup(func() {
				introspected.Request = savedRequest
				introspected.StreamRequest = savedStreamRequest
			})

			introspected.Request = nil
			introspected.StreamRequest = nil
			tc.set()

			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("expected panic, got none — generate should reject SupportsWithClient=false with non-nil BuildRequest singletons")
				}
				msg, ok := r.(string)
				if !ok {
					t.Fatalf("expected string panic value, got %T: %v", r, r)
				}
				// Message must mention SupportsWithClient and the
				// case-specific singleton state.
				if !strings.Contains(msg, "SupportsWithClient") {
					t.Errorf("panic message missing SupportsWithClient: %q", msg)
				}
				if !strings.Contains(msg, tc.wantSubstr) {
					t.Errorf("panic message missing case-specific singleton state %q: %q", tc.wantSubstr, msg)
				}
			}()

			generate(Options{
				SelfPkg:            "github.com/invakid404/baml-rest/adapters/test_invalid",
				SupportsWithClient: false,
			})
		})
	}
}

// The inverse-regression guard (legacy-only adapters must NOT trip
// the panic) is implicitly covered by the existing v0.204/v0.215
// adapter tests passing — both run with SupportsWithClient=false
// against introspected singletons that remain nil in those build
// configurations. Adding a same-package positive test here would run
// generate() to completion and write adapter.go into the codegen
// package directory, polluting the repo.
