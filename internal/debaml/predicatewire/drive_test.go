//go:build integration

package predicatewire

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/boundaryml/baml/engine/language_client_go/baml_go/shared"
	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
	"github.com/bytedance/sonic"
)

// Driving the stock CFFI.
//
// One parse per (project, function, raw text). Both halves of the result are retained:
// a drive is either a value capture or an error capture, and which one it is is part of
// what gets pinned.

// pwDriveKey identifies one drive.
type pwDriveKey struct {
	Project string
	Func    string
	Raw     string
}

func (k pwDriveKey) String() string {
	return fmt.Sprintf("%s/%s(%s)", k.Project, pwFuncPrefix+k.Func, k.Raw)
}

// pwResult is one stock parse.
type pwResult struct {
	value any
	err   error
}

// pwCache memoizes one parse per key. No mutex: every subtest here is sequential (the
// CFFI runtimes are process-global state and this suite deliberately does not
// parallelise over them).
var pwCache = map[pwDriveKey]pwResult{}

// pwDrive runs one drive through the stock CFFI.
//
// The progress marker goes to STDERR unbuffered before the call, so if a row takes the
// process down the last marker names the row that did it.
func pwDrive(t *testing.T, key pwDriveKey) pwResult {
	t.Helper()
	if r, ok := pwCache[key]; ok {
		return r
	}
	rt := pwRuntimeOf(t, key.Project)
	fmt.Fprintf(os.Stderr, "predicate-wire: stock BEGIN %s\n", key)
	args := baml.BamlFunctionArguments{
		Kwargs: map[string]any{"text": key.Raw, "stream": false},
		Env:    pwEnv,
	}
	encoded, err := args.Encode()
	if err != nil {
		t.Fatalf("%s: encode arguments: %v", key, err)
	}
	value, callErr := rt.CallFunctionParse(context.Background(), pwFuncPrefix+key.Func, encoded)
	fmt.Fprintf(os.Stderr, "predicate-wire: stock END   %s\n", key)
	r := pwResult{value: value, err: callErr}
	pwCache[key] = r
	return r
}

// pwValue returns the decoded value for a drive that must SUCCEED.
func pwValue(t *testing.T, key pwDriveKey) any {
	t.Helper()
	r := pwDrive(t, key)
	if r.err != nil {
		t.Fatalf("%s: stock parse failed, but this drive is a VALUE capture: %v", key, r.err)
	}
	return r.value
}

// pwError returns the UNMODIFIED error for a drive that must FAIL.
func pwError(t *testing.T, key pwDriveKey) error {
	t.Helper()
	r := pwDrive(t, key)
	if r.err == nil {
		t.Fatalf("%s: stock parse SUCCEEDED (%#v), but this drive is an ERROR capture", key, r.value)
	}
	if r.value != nil {
		t.Fatalf("%s: stock produced BOTH an error and a value (%#v); a failed @assert emits no value",
			key, r.value)
	}
	return r.err
}

// pwCheckedValue pulls a decoded value out of a drive and requires it to be the concrete
// generated CHECK-family shape.
//
// The type assertion is the point: if stock had NOT wrapped `confidence`, or had wrapped
// it in something else, this fails rather than silently comparing a different shape.
func pwCheckedValue(t *testing.T, key pwDriveKey) pwCheckedAnswer {
	t.Helper()
	v := pwValue(t, key)
	c, ok := v.(pwCheckedAnswer)
	if !ok {
		t.Fatalf("%s: stock decoded a %T, want %s", key, v, pwCheckedClass)
	}
	return c
}

// pwAssertValue is the ASSERT-family twin.
func pwAssertValue(t *testing.T, key pwDriveKey) pwAssertAnswer {
	t.Helper()
	v := pwValue(t, key)
	a, ok := v.(pwAssertAnswer)
	if !ok {
		t.Fatalf("%s: stock decoded a %T, want %s", key, v, pwAssertClass)
	}
	return a
}

// pwBareChecked is the bare-`int`-target twin: a probe whose target carries the
// constraint directly decodes to the generated carrier itself.
func pwBareChecked(t *testing.T, key pwDriveKey) shared.Checked[int64] {
	t.Helper()
	v := pwValue(t, key)
	c, ok := v.(shared.Checked[int64])
	if !ok {
		t.Fatalf("%s: stock decoded a %T, want shared.Checked[int64]", key, v)
	}
	return c
}

// pwRequireSonicBytes asserts sonic.Marshal(v) is exactly want.
//
// sonic is the WIRE AUTHORITY: it is the serializer worker/parse.go and the final stream
// path use. encoding/json is not interchangeable — it HTML-escapes the output of any
// json.Marshaler, and every canonical expression here carries a `<`, `>` or both — which
// checkedwire's TestStockWireEncoderFraming measures on stock's own struct.
func pwRequireSonicBytes(t *testing.T, what string, v any, want string) {
	t.Helper()
	got, err := sonic.Marshal(v)
	if err != nil {
		t.Fatalf("%s: sonic.Marshal: %v", what, err)
	}
	if string(got) != want {
		t.Fatalf("%s: sonic bytes:\n got %s\nwant %s", what, got, want)
	}
}

// pwAllDrives is EVERY drive this package performs, assembled from the row tables.
//
// It exists so whole-table guards ([TestPredicateWireDecodersSawWhatWasDeclared], and the
// coverage assertion in [TestOperatorManifestIsTheWholeGrammar]) see every row rather
// than the rows one test happened to reach.
func pwAllDrives() []pwDriveKey {
	var out []pwDriveKey
	for _, o := range pwOperators() {
		for _, v := range []int64{o.TrueVal, o.FalseVal} {
			out = append(out,
				pwDriveKey{Project: o.projectKey(), Func: pwFnChecked, Raw: pwNestedRaw(v)},
				pwDriveKey{Project: o.projectKey(), Func: pwFnAssert, Raw: pwNestedRaw(v)},
			)
		}
	}
	// The TOP-LEVEL twins of the operator matrix. A bare target takes the raw integer
	// text directly rather than the two-field object.
	for _, o := range pwOperators() {
		for _, v := range []int64{o.TrueVal, o.FalseVal} {
			raw := strconv.FormatInt(v, 10)
			out = append(out,
				pwDriveKey{Project: pwTopLevelKey, Func: pwTopCheckFn(o), Raw: raw},
				pwDriveKey{Project: pwTopLevelKey, Func: pwTopAssertFn(o), Raw: raw},
			)
		}
	}
	for _, probe := range pwPadProbes() {
		out = append(out, pwDriveKey{Project: pwExprTextKey, Func: "Pad_" + probe.Label, Raw: "5"})
	}
	for _, p := range pwLiteralProbes() {
		// A project stock REFUSES has no function to drive; its capture is the refusal,
		// asserted by TestStockNonCanonicalLiteralDispositions through pwCompileError.
		if p.Rejected {
			continue
		}
		out = append(out, pwDriveKey{Project: p.projectKey(), Func: pwFnChecked, Raw: pwNestedRaw(p.Confidence)})
	}
	for _, b := range pwBoundaryThresholds() {
		for _, v := range pwBoundaryValues(b.N) {
			out = append(out, pwDriveKey{Project: b.projectKey(), Func: pwBoundaryFn, Raw: fmt.Sprint(v)})
		}
	}
	for _, r := range pwResiduals() {
		for _, v := range r.Drives {
			out = append(out, pwDriveKey{Project: r.projectKey(), Func: r.fn(), Raw: pwNestedRaw(v)})
		}
	}
	return out
}

// pwDriveEveryRow drives every row once.
func pwDriveEveryRow(t *testing.T) {
	t.Helper()
	for _, key := range pwAllDrives() {
		pwDrive(t, key)
	}
}
