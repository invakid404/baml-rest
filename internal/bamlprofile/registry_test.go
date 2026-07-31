package bamlprofile

import (
	"errors"
	"testing"

	minijinja "github.com/invakid404/minijinja-go/v2"
)

// This is the effective-registry inventory the Slice 2 scope requires: a
// snapshot that the environment New() builds registers EXACTLY BAML's get_env
// filter/test/function set — the fork's BAML-aligned default registry
// (defaults.go, "exactly the builtin set BAML's engine build enables") plus the
// profile's get_env addition `regex_match`, and NONE of the five names the Go
// port shipped that BAML's engine build withdraws (urlencode, containing,
// cycler, joiner, lipsum; fork defaults.go:14-35). It is pure-Go: it probes the
// live environment rather than assuming defaults stay aligned, so a fork bump
// that adds/removes a builtin — or a profile change that drops regex_match — is
// caught as drift here rather than surfacing far from the call site.
//
// The registries are unexported on the fork's Environment, so presence is probed
// behaviorally: an unregistered name renders as *minijinja.Error with the
// unknown-filter/test/function kind (verified), while a registered name renders
// OK or fails for some OTHER reason (wrong arg/type) — never unknown-*.

// probeErr renders a probe template and returns the first error (compile or
// render), or nil.
func probeErr(t *testing.T, src string) error {
	t.Helper()
	tmpl, cerr := New(Config{}).TemplateFromNamedString("registry_probe", src)
	if cerr != nil {
		return cerr
	}
	_, rerr := tmpl.Render(map[string]any{})
	return rerr
}

func isKind(err error, kind minijinja.ErrorKind) bool {
	var me *minijinja.Error
	return errors.As(err, &me) && me.Kind == kind
}

// registered filters BAML's get_env exposes: the fork default registry
// (defaults.go registerDefaultFilters) plus the profile's regex_match. `format`
// is present at get_env level as the engine builtin; BAML's render layer
// overrides it (a later slice), which is a value change, not a registry change.
var wantFilters = []string{
	"upper", "lower", "capitalize", "title", "trim", "replace", "format",
	"default", "d", "safe", "escape", "e", "string", "bool", "split", "lines",
	"length", "count", "first", "last", "reverse", "sort", "join", "list",
	"unique", "min", "max", "sum", "batch", "slice", "map", "select", "reject",
	"selectattr", "rejectattr", "groupby", "chain", "zip", "abs", "int", "float",
	"round", "items", "dictsort", "attr", "indent", "pprint", "tojson",
	"regex_match", // profile get_env addition
}

// registered tests (defaults.go registerDefaultTests), excluding the operator
// aliases (==, !=, <, <=, >, >=) which are invoked as operators, not `is NAME`.
var wantTests = []string{
	"defined", "undefined", "none", "true", "false", "odd", "even",
	"divisibleby", "eq", "equalto", "ne", "lt", "lessthan", "le", "gt",
	"greaterthan", "ge", "in", "string", "number", "integer", "int", "float",
	"boolean", "sequence", "mapping", "iterable", "startingwith", "endingwith",
	"safe", "escaped", "sameas", "lower", "upper", "filter", "test",
}

// registered functions (defaults.go registerDefaultFunctions).
var wantFunctions = []string{"range", "dict", "namespace", "debug"}

// withdrawn: the five names the Go port shipped that BAML's engine build does
// NOT enable, so they must be ABSENT (fork defaults.go:14-35).
var (
	withdrawnFilters   = []string{"urlencode"}
	withdrawnTests     = []string{"containing"}
	withdrawnFunctions = []string{"cycler", "joiner", "lipsum"}
)

func TestRegistryFiltersPresent(t *testing.T) {
	for _, name := range wantFilters {
		if isKind(probeErr(t, `{{ "x"|`+name+` }}`), minijinja.ErrUnknownFilter) {
			t.Errorf("expected filter %q to be registered, but it is unknown", name)
		}
	}
	for _, name := range withdrawnFilters {
		if !isKind(probeErr(t, `{{ "x"|`+name+` }}`), minijinja.ErrUnknownFilter) {
			t.Errorf("filter %q must be withdrawn (BAML does not enable it), but it is registered", name)
		}
	}
}

func TestRegistryTestsPresent(t *testing.T) {
	for _, name := range wantTests {
		if isKind(probeErr(t, `{{ "x" is `+name+` }}`), minijinja.ErrUnknownTest) {
			t.Errorf("expected test %q to be registered, but it is unknown", name)
		}
	}
	for _, name := range withdrawnTests {
		if !isKind(probeErr(t, `{{ "x" is `+name+` }}`), minijinja.ErrUnknownTest) {
			t.Errorf("test %q must be withdrawn, but it is registered", name)
		}
	}
}

func TestRegistryFunctionsPresent(t *testing.T) {
	for _, name := range wantFunctions {
		if isKind(probeErr(t, `{{ `+name+`() }}`), minijinja.ErrUnknownFunction) {
			t.Errorf("expected function %q to be registered, but it is unknown", name)
		}
	}
	for _, name := range withdrawnFunctions {
		if !isKind(probeErr(t, `{{ `+name+`() }}`), minijinja.ErrUnknownFunction) {
			t.Errorf("function %q must be withdrawn, but it is registered", name)
		}
	}
}
