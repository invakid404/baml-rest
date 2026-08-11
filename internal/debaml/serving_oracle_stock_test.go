//go:build integration

package debaml

// The STOCK leg: an in-memory .baml project compiled by the stock BAML v0.223.0
// CFFI and driven through BamlRuntime.CallFunctionParse.
//
// WHY CallFunctionParse RATHER THAN A GENERATED Parse.<Method>. They are the same
// call: every generated `func (*parse) FooFn(text string)` encodes
// {"text": …, "stream": false} and calls exactly this runtime method. Going one
// layer down buys two things this slice needs — the project can be built in
// memory from the corpus instead of checked in and regenerated, and the RAW CFFI
// value tree is reachable, which is where the ordered check collection lives.
//
// HOW THE RAW TREE IS REACHED. baml_go decodes the CFFI payload on the callback
// thread using a process-global type map. Registering the corpus's class names to
// [soClassObserver] — a type whose Decode does nothing but retain the protobuf
// pointer — makes that decode hand back the tree itself. Nothing else is done on
// the callback thread: a panic there is not recoverable by the caller (it is the
// same property that makes `divisibleby(0)` process-fatal), so the observer is the
// smallest possible body and every walk happens on the test goroutine.
//
// NATIVE OUTPUT IS NEVER FED BACK IN. The only bytes handed to the CFFI are a
// fixture's Raw assistant text. The native leg's canonical JSON, its
// ConstraintValue and its events are compared against what stock produced and are
// never used to construct a stock call.

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	baml_go "github.com/boundaryml/baml/engine/language_client_go/baml_go"
	"github.com/boundaryml/baml/engine/language_client_go/baml_go/serde"
	"github.com/boundaryml/baml/engine/language_client_go/baml_go/shared"
	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
	"github.com/boundaryml/baml/engine/language_client_go/pkg/cffi"

	"github.com/invakid404/baml-rest/internal/schema"
)

// soClassObserver is registered for every corpus class. Its Decode retains the
// protobuf node and returns; see the file comment for why it does nothing else.
type soClassObserver struct{ raw *cffi.CFFIValueClass }

func (o *soClassObserver) Decode(h *cffi.CFFIValueClass, _ baml.TypeMap) { o.raw = h }

// soEnumObserver is the same for enums.
type soEnumObserver struct{ raw *cffi.CFFIValueEnum }

func (o *soEnumObserver) Decode(h *cffi.CFFIValueEnum) { o.raw = h }

// soChecked is the shape baml_go's decodeCheckedValue requires by REFLECTION: it
// sets the fields named Value and Checks and nothing else, so the field names and
// the Checks type are load-bearing.
//
// # The one place the raw tree is NOT reachable, and what is done about it
//
// It is reached only for a Checked value at the ROOT of the returned document (a
// target-level constraint, a root class-level @@check, or a root list/map whose
// elements carry one). Every NESTED Checked is captured as a raw holder by an
// observer above it, ordered collection intact.
//
// At the root there is no hook: baml_go decodes the payload on the CFFI callback
// thread and `decodeCheckedValue` has already folded `[]CFFICheckValue` into a
// `map[string]shared.Check` before any registered type is touched — it only
// reflect-Sets the fields named Value and Checks. The lower-level entry points
// (baml_go.CallFunctionParseFromC + RegisterCallbacks) would hand back the raw
// protobuf, but registering a callback needs `import "C"`, which Go does not allow
// in a _test.go file. So the fold is a property of the binding, not a choice here.
//
// So at a ROOT position stock's ORDER and MULTIPLICITY are UNAVAILABLE to this
// oracle. That is stated rather than worked around, and it is NOT reconstructed
// from the schema and presented as observed:
//
//   - every site recovered from a folded root map is marked Certified=false and
//     renders as `~uncertified-order`, which is part of the PINNED stock envelope,
//     so the five affected corpus rows record the limitation in the corpus itself;
//   - the comparator does not index-pair those sites. It matches them as an
//     UNORDERED multiset per path (soCompareUncertifiedSites), so no ordering claim
//     rests on evidence that no longer exists, and it records the unavailability on
//     the row's report;
//   - what the folded map DOES evidence is still checked exactly: for every entry
//     stock REPORTED, its label must be declared at that node, its expression text
//     must match the declaration, and the result of each matched pair is compared —
//     so an entry stock reported that the schema does not declare is a hard failure,
//     and so is native deciding a predicate stock's collection does not contain. A
//     declared constraint the map does NOT report is deliberately PERMITTED and is
//     not a refusal: that is what a losing union arm and a route that skips
//     evaluation look like (see soFoldedSites, which continues past an absent
//     entry); and
//   - a node whose schema declares one label TWICE is refused UNCONDITIONALLY by
//     soFoldedSites, whatever the map turned out to contain, because a map-keyed
//     readback cannot represent that result at all.
//
// TestServingOracleRootCheckFoldIsLossy measures the loss rather than describing
// it: it drives a root duplicate-label probe through the real CFFI and observes the
// decoded map keep ONE entry where the .baml declares two, with the NESTED twin
// keeping both, in declaration order, from the raw tree.
// TestServingOracleRootEnvelopeCertificationIsUnavailable then asserts the
// unavailability itself — that uncertified sites exist, that certified ones also
// exist so the limitation is narrow, that only ROOT paths are ever uncertified, and
// that every affected row's pinned envelope carries the marker.
type soChecked struct {
	Value  any
	Checks map[string]shared.Check
}

// ---------------------------------------------------------------------------
// The type map
// ---------------------------------------------------------------------------

// soFixedCheckedNames are the CHECKED_TYPES keys baml_go can ask for that are not
// derived from a corpus declaration.
//
// Two different namings reach the same map, and both are registered because both
// are live: baml_go's serde.typeToString (which renders a container as the bare
// word "list"/"optional"/"union"/"class") is used when a LIST or MAP element type
// is converted, while the Rust side sends compound value names ("List__int",
// "Optional__int") on the value itself. A missing key is a panic on the callback
// thread, i.e. a dead process, so this registration is deliberately generous —
// over-registration costs nothing because every key maps to the same observer.
var soFixedCheckedNames = []string{
	"int", "string", "float", "bool", "null",
	"class", "list", "map", "optional", "union", "checked", "stream_state",
}

// soCheckedValueName renders the compound CHECKED_TYPES name the Rust side sends
// for a value of type t, mirroring the names observed in this repo's checked-in
// stock client (List__int, Optional__int, Map__string_int).
func soCheckedValueName(t schema.Type) string {
	switch t.Kind {
	case schema.TypePrimitive:
		switch t.Primitive {
		case schema.PrimitiveString:
			return "string"
		case schema.PrimitiveInt:
			return "int"
		case schema.PrimitiveFloat:
			return "float"
		case schema.PrimitiveBool:
			return "bool"
		case schema.PrimitiveNull:
			return "null"
		}
		return string(t.Primitive)
	case schema.TypeEnum, schema.TypeClass:
		return t.Name
	case schema.TypeList:
		if t.Elem == nil {
			return "List__unknown"
		}
		return "List__" + soCheckedValueName(*t.Elem)
	case schema.TypeMap:
		if t.Key == nil || t.Value == nil {
			return "Map__unknown"
		}
		return "Map__" + soCheckedValueName(*t.Key) + "_" + soCheckedValueName(*t.Value)
	case schema.TypeUnion:
		if t.Union != nil && t.Union.Nullable && len(t.Union.Variants) == 1 {
			return "Optional__" + soCheckedValueName(t.Union.Variants[0])
		}
		return "union"
	}
	return string(t.Kind)
}

// soWalkTypes yields t and every type nested inside it.
func soWalkTypes(t schema.Type, fn func(schema.Type)) {
	fn(t)
	if t.Elem != nil {
		soWalkTypes(*t.Elem, fn)
	}
	if t.Key != nil {
		soWalkTypes(*t.Key, fn)
	}
	if t.Value != nil {
		soWalkTypes(*t.Value, fn)
	}
	for _, it := range t.Items {
		soWalkTypes(it, fn)
	}
	if t.Union != nil {
		for _, v := range t.Union.Variants {
			soWalkTypes(v, fn)
		}
	}
	if t.Arrow != nil {
		for _, p := range t.Arrow.Params {
			soWalkTypes(p, fn)
		}
		soWalkTypes(t.Arrow.Return, fn)
	}
}

// soBuildTypeMap derives the process-global BAML type map from the corpus.
//
// extra carries bundles that are driven through the CFFI but are not corpus rows —
// today the 719-case bridge's generated units. They MUST be registered here: an
// unregistered class decodes dynamically and its inner Checked then panics on the
// CFFI callback thread, which kills the process rather than failing a test.
func soBuildTypeMap(fixtures []servingOracleFixture, extra []*schema.Bundle) map[string]reflect.Type {
	clsT := reflect.TypeOf(soClassObserver{})
	enumT := reflect.TypeOf(soEnumObserver{})
	chkT := reflect.TypeOf(soChecked{})

	tm := map[string]reflect.Type{}
	for _, n := range soFixedCheckedNames {
		tm["CHECKED_TYPES."+n] = chkT
	}
	// The probes are rendered into the same project and driven through the same
	// CFFI, so their declarations need the same registration; without it a probe's
	// class decodes dynamically and its raw tree — the control the root-fold
	// measurement rests on — is lost.
	for _, pr := range servingOracleProbes {
		for _, c := range pr.Bundle.Classes {
			tm["TYPES."+c.Name.Name] = clsT
			tm["CHECKED_TYPES."+c.Name.Name] = chkT
			for _, fld := range c.Fields {
				soWalkTypes(fld.Type, func(t schema.Type) {
					tm["CHECKED_TYPES."+soCheckedValueName(t)] = chkT
				})
			}
		}
		soWalkTypes(pr.Bundle.Target, func(t schema.Type) {
			tm["CHECKED_TYPES."+soCheckedValueName(t)] = chkT
		})
	}
	for _, b := range extra {
		if b == nil {
			continue
		}
		for _, c := range b.Classes {
			tm["TYPES."+c.Name.Name] = clsT
			tm["CHECKED_TYPES."+c.Name.Name] = chkT
			for _, fld := range c.Fields {
				soWalkTypes(fld.Type, func(t schema.Type) {
					tm["CHECKED_TYPES."+soCheckedValueName(t)] = chkT
				})
			}
		}
		for _, e := range b.Enums {
			tm["TYPES."+e.Name.Name] = enumT
			tm["CHECKED_TYPES."+e.Name.Name] = chkT
		}
		soWalkTypes(b.Target, func(t schema.Type) {
			tm["CHECKED_TYPES."+soCheckedValueName(t)] = chkT
		})
	}
	for _, f := range fixtures {
		if f.Bundle == nil {
			continue
		}
		for _, c := range f.Bundle.Classes {
			tm["TYPES."+c.Name.Name] = clsT
			tm["CHECKED_TYPES."+c.Name.Name] = chkT
			for _, fld := range c.Fields {
				soWalkTypes(fld.Type, func(t schema.Type) {
					tm["CHECKED_TYPES."+soCheckedValueName(t)] = chkT
				})
			}
		}
		for _, e := range f.Bundle.Enums {
			tm["TYPES."+e.Name.Name] = enumT
			tm["CHECKED_TYPES."+e.Name.Name] = chkT
		}
		soWalkTypes(f.Bundle.Target, func(t schema.Type) {
			tm["CHECKED_TYPES."+soCheckedValueName(t)] = chkT
		})
	}
	return tm
}

// ---------------------------------------------------------------------------
// The runtime
// ---------------------------------------------------------------------------

var (
	soRuntimeOnce sync.Once
	soRuntime     baml.BamlRuntime
	soRuntimeErr  error
	// soRuntimeSource is the EXACT project text the runtime was created from. The
	// drift test compares this against the golden rather than re-rendering, so the
	// bytes stock compiled are the bytes under review.
	soRuntimeSource string
	soRuntimeEnv    map[string]string
	soRuntimeTypes  map[string]reflect.Type
)

// soEnsureRuntime renders the project, registers the type map and creates the
// stock runtime exactly once per process.
func soEnsureRuntime(t *testing.T) {
	t.Helper()
	soRuntimeOnce.Do(func() {
		soRuntimeSource, soRuntimeErr = soRenderProject(servingOracleFixtures)
		if soRuntimeErr != nil {
			return
		}
		// The 719-case bridge shares this process's global type map, so its generated
		// units are registered here even though they are driven through their own
		// runtime. Loading the artifact up front also means a missing or malformed one
		// is a loud failure at setup rather than a panic mid-run.
		var extra []*schema.Bundle
		soBridgeUnits, soRuntimeErr = soBridgeUnitsFromArtifact()
		if soRuntimeErr != nil {
			return
		}
		for _, u := range soBridgeUnits {
			extra = append(extra, u.Bundle)
		}
		soRuntimeTypes = soBuildTypeMap(servingOracleFixtures, extra)
		baml.SetTypeMap(soRuntimeTypes)
		soRuntimeEnv = soEnvSnapshot()
		soRuntime, soRuntimeErr = baml.CreateRuntime("./baml_src", soProjectFiles(soRuntimeSource), soRuntimeEnv)
	})
	if soRuntimeErr != nil {
		t.Fatalf("stock runtime for the in-memory serving-oracle project could not be created: %v\n"+
			"the project the corpus renders is not a valid BAML v0.223.0 project:\n%s", soRuntimeErr, soRuntimeSource)
	}
}

// soDriveStock runs one fixture through the stock CFFI and returns its envelope.
//
// The progress marker goes to STDERR unbuffered before the call. If a row aborts
// the process — the failure mode `divisibleby(0)` has — the last marker names the
// row that did it, so an unpinned fatal cannot present as an unattributable crash.
func soDriveStock(f servingOracleFixture) (soStockEnvelope, error) {
	fmt.Fprintf(os.Stderr, "serving-oracle: stock BEGIN %s\n", f.method())
	args := baml.BamlFunctionArguments{
		Kwargs: map[string]any{"text": f.Raw, "stream": false},
		Env:    soRuntimeEnv,
	}
	encoded, err := args.Encode()
	if err != nil {
		return soStockEnvelope{}, fmt.Errorf("encode arguments for %s: %w", f.method(), err)
	}
	res, callErr := soRuntime.CallFunctionParse(context.Background(), f.method(), encoded)
	fmt.Fprintf(os.Stderr, "serving-oracle: stock END   %s\n", f.method())
	if callErr != nil {
		return soClassifyStockError(callErr), nil
	}
	return soStockEnvelopeFromResult(f, res)
}

// soStockEnvelopeFromResult turns a decoded CallFunctionParse result into the
// comparison envelope.
func soStockEnvelopeFromResult(f servingOracleFixture, res any) (soStockEnvelope, error) {
	var sites []soStockSite
	identity, doc, err := soReadStockResult(f.Bundle, res, "$", &sites)
	if err != nil {
		return soStockEnvelope{}, fmt.Errorf("%s: read stock value: %w", f.method(), err)
	}
	return soStockEnvelope{Kind: soStockValue, Identity: identity, JSON: doc, Sites: sites}, nil
}

// soReadStockResult bridges the DECODED root (which is whatever the global type
// map produced) into the raw-tree walk.
//
// Only three root shapes can occur, and each is handled explicitly rather than
// through a default: an observer (the raw tree is available), a root-level
// Checked or a slice of them (baml_go decoded them before this code ran), and a
// bare scalar for an unconstrained scalar return.
func soReadStockResult(b *schema.Bundle, res any, path string, sites *[]soStockSite) (string, string, error) {
	switch v := res.(type) {
	case soClassObserver:
		if v.raw == nil {
			return "", "", fmt.Errorf("%s: class observer captured no value", path)
		}
		id, doc, err := soReadStockValue(&cffi.CFFIValueHolder{
			Value: &cffi.CFFIValueHolder_ClassValue{ClassValue: v.raw},
		}, path, sites)
		return id, string(doc), err
	case soEnumObserver:
		if v.raw == nil {
			return "", "", fmt.Errorf("%s: enum observer captured no value", path)
		}
		id, doc, err := soReadStockValue(&cffi.CFFIValueHolder{
			Value: &cffi.CFFIValueHolder_EnumValue{EnumValue: v.raw},
		}, path, sites)
		return id, string(doc), err
	case soChecked:
		// A ROOT-level Checked. baml_go has already folded the check collection into
		// a map here, so the sites are recovered from that map and the fold is
		// recorded rather than hidden: soCheckedSites sorts by label so the ordering
		// claim is never made from data that no longer carries order.
		folded, err := soFoldedSites(b, path, v.Checks)
		if err != nil {
			return "", "", err
		}
		*sites = append(*sites, folded...)
		return soReadStockResult(b, v.Value, path, sites)
	case []soChecked:
		ids := make([]string, len(v))
		docs := make([]string, len(v))
		for i, e := range v {
			id, doc, err := soReadStockResult(b, e, fmt.Sprintf("%s[%d]", path, i), sites)
			if err != nil {
				return "", "", err
			}
			ids[i], docs[i] = id, doc
		}
		return "list[" + strings.Join(ids, ",") + "]", "[" + strings.Join(docs, ",") + "]", nil
	case serde.DynamicUnion:
		// A union at the ROOT. baml_go decodes it before this code runs and keeps the
		// winning variant's NAME plus its value. The union wrapper is transparent to
		// the identity — which arm won is visible in the value's own rendering — so
		// only the inner value is walked.
		return soReadStockResult(b, v.Value, path, sites)
	case serde.DynamicClass:
		return "", "", fmt.Errorf("%s: stock decoded the class %q DYNAMICALLY, so its raw tree (and its "+
			"ordered check collection) was lost; register TYPES.%s in the oracle type map", path, v.Name, v.Name)
	case serde.DynamicEnum:
		return "", "", fmt.Errorf("%s: stock decoded the enum %q DYNAMICALLY; register TYPES.%s in the "+
			"oracle type map", path, v.Name, v.Name)
	case string:
		return "string:" + fmt.Sprintf("%q", v), fmt.Sprintf("%q", v), nil
	case int64:
		return fmt.Sprintf("int:%d", v), fmt.Sprintf("%d", v), nil
	case float64:
		f := strconv.FormatFloat(v, 'g', -1, 64)
		return "float:" + f, f, nil
	case bool:
		return fmt.Sprintf("bool:%v", v), fmt.Sprintf("%v", v), nil
	case nil:
		return "null", "null", nil
	}
	return "", "", fmt.Errorf("%s: unmodelled stock root %T", path, res)
}

// soFoldedSites recovers what CAN be recovered from a FOLDED root check map, and
// marks it UNCERTIFIED.
//
// # Root-envelope certification is UNAVAILABLE, and is not simulated
//
// baml_go folds a root Checked's collection into map[string]shared.Check inside
// serde.decodeCheckedValue, on the CFFI callback thread, before any registered type
// is touched (see soChecked). Stock's ORDER and MULTIPLICITY at a root position are
// therefore destroyed before this oracle can observe them, and no amount of
// schema-side reasoning recovers them: the schema says what was DECLARED, not what
// stock reported.
//
// So this does NOT present a certified root envelope. It emits sites with
// Certified=false, which:
//
//   - renders as `~uncertified-order` inside the PINNED stock envelope, so a row
//     whose root evidence is uncertified says so in the corpus; and
//   - makes the comparator compare those sites as an unordered multiset per path
//     rather than claiming an order it did not observe.
//
// What IS still verified exactly, because the folded map does carry it: every label
// stock reported must be declared at that node with the SAME expression text, and
// the counts must match, so a reported entry the schema does not declare, or a
// declared-and-reported entry that went missing, is a hard failure.
//
// # A determinable duplicate is refused UNCONDITIONALLY
//
// If the schema declares one label more than once at this node, the fold cannot
// represent the result whatever the map happens to contain — including when the map
// is EMPTY, or when the repeated label is absent while another is present. The
// refusal below is therefore driven by the DECLARATION alone and never by the map's
// contents; making it conditional on the map is what let those two states through
// before.
func soFoldedSites(b *schema.Bundle, path string, checks map[string]shared.Check) ([]soStockSite, error) {
	declared, err := soDeclaredConstraintsAt(b, path)
	if err != nil {
		return nil, err
	}
	// UNCONDITIONAL: a label declared more than once at this node cannot survive a
	// map-keyed readback, whatever the map turned out to contain. Checking the map
	// first would accept the two states where the loss is invisible — an empty map,
	// and a map whose surviving entry is a DIFFERENT label.
	byLabel := map[string]int{}
	labelOrder := []string{}
	for _, c := range declared {
		if c.Label == nil {
			continue
		}
		if byLabel[*c.Label] == 0 {
			labelOrder = append(labelOrder, *c.Label)
		}
		byLabel[*c.Label]++
	}
	for _, label := range labelOrder {
		if byLabel[label] < 2 {
			continue
		}
		return nil, fmt.Errorf("%s: the schema declares %d constraints under the label %q, and baml_go's "+
			"root readback folds the check collection into a map[string]Check — so at most one of them "+
			"survives and the root envelope is LOST for this node, whatever the map happens to contain "+
			"(it reported %d entry/entries: %v). A fixture cannot be compared here; drive the shape as a "+
			"probe instead (see TestServingOracleRootCheckFoldIsLossy).",
			path, byLabel[label], label, len(checks), checks)
	}

	var out []soStockSite
	matched := 0
	for _, c := range declared {
		if c.Label == nil {
			continue
		}
		got, ok := checks[*c.Label]
		if !ok {
			// Declared but not reported: legitimate for a union's losing arm, and for
			// a route that skips evaluation. The count check below is what makes a
			// LOSS visible.
			continue
		}
		matched++
		if got.Expression != c.Expression {
			return nil, fmt.Errorf("%s: stock reported the label %q with the expression %q, but the schema "+
				"declares %q at that node", path, *c.Label, got.Expression, c.Expression)
		}
		// UNCERTIFIED: the position is the node's real path, but the ORDER among
		// sibling entries is the schema's rather than stock's, because the fold
		// destroyed stock's.
		out = append(out, soStockSite{Path: path, Label: got.Name, Expression: got.Expression,
			Status: got.Status, Certified: false})
	}
	if matched != len(checks) {
		return nil, fmt.Errorf("%s: stock reported %d folded check(s) but %d of the %d constraint(s) the "+
			"schema declares there matched. baml_go's root readback folds the collection into a "+
			"map[string]Check, so this is what a LOST entry (two @check attributes under one label) looks "+
			"like — the root envelope is no longer exact and the fixture cannot be compared. Reported: %v",
			path, len(checks), matched, len(declared), checks)
	}
	return out, nil
}

// soDeclaredConstraintsAt returns the constraints the SCHEMA declares at a root
// path, in the order the collector attaches them (declaration site first, then the
// type node).
//
// It handles only the three ROOT positions a folded readback can occur at. Every
// nested site comes from the raw CFFI tree and never needs this, so an unexpected
// path is an error rather than an empty list — an empty list would silently make
// the count check above vacuous.
func soDeclaredConstraintsAt(b *schema.Bundle, path string) ([]schema.Constraint, error) {
	node := b.Target
	switch {
	case path == "$":
	case strings.HasPrefix(path, "$[") && strings.HasSuffix(path, "]"):
		inner := path[2 : len(path)-1]
		if strings.HasPrefix(inner, `"`) {
			if b.Target.Value == nil {
				return nil, fmt.Errorf("%s: the target is not a map, so a map-entry path is impossible", path)
			}
			node = *b.Target.Value
		} else {
			if b.Target.Elem == nil {
				return nil, fmt.Errorf("%s: the target is not a list, so an index path is impossible", path)
			}
			node = *b.Target.Elem
		}
	default:
		return nil, fmt.Errorf("%s: a folded check collection can only occur at a ROOT position "+
			"($, $[i] or $[\"k\"]); a nested one is read from the raw CFFI tree", path)
	}

	var out []schema.Constraint
	// A UNION node carries its constraints on the winning arm; which arm won is not
	// knowable from the schema alone, so every arm's constraints are offered and the
	// label match above selects the winner's. The count check still makes a loss
	// visible, because a lost entry lowers the match count.
	if node.Kind == schema.TypeUnion && node.Union != nil {
		for _, v := range node.Union.Variants {
			out = append(out, soDeclaredAtNode(b, v)...)
		}
		out = append(out, node.Meta.Constraints...)
		return out, nil
	}
	return soDeclaredAtNode(b, node), nil
}

// soDeclaredAtNode is the per-node half: the class/enum DECLARATION's constraints
// first, then the TYPE node's own, mirroring the collector's attachment order.
func soDeclaredAtNode(b *schema.Bundle, t schema.Type) []schema.Constraint {
	var out []schema.Constraint
	switch t.Kind {
	case schema.TypeClass:
		if cls, ok := b.FindClass(t.Name, t.Mode); ok {
			out = append(out, cls.Constraints...)
		}
	case schema.TypeEnum:
		if en, ok := b.FindEnum(t.Name); ok {
			out = append(out, en.Constraints...)
		}
	}
	return append(out, t.Meta.Constraints...)
}

// soBAMLRuntimeVersion is the CFFI version the whole oracle is only valid against.
const soBAMLRuntimeVersion = "0.223.0"

// soLoadedBAMLVersion reports the version of the native library actually loaded
// into this process.
func soLoadedBAMLVersion() string { return baml_go.BamlVersion() }

// soCreateRuntimeForSource compiles a one-file project and returns the runtime.
// It is used by the source-rejection test, which needs a project that must FAIL to
// compile, so it deliberately does not go through the memoized corpus runtime.
func soCreateRuntimeForSource(src string) (baml.BamlRuntime, error) {
	return baml.CreateRuntime("./baml_src", map[string]string{soProjectFile: src}, soEnvSnapshot())
}

// TestServingOracleStockReadbackIsStrict proves the readback's own refusals BITE
// rather than decorate.
//
// Neither shape can be produced by stock BAML today, which is exactly why they are
// driven directly: a guard that no fixture can reach is indistinguishable from one
// that is not there, and a duplicate key or field silently kept would make the
// canonical document depend on which entry the walk happened to record last.
func TestServingOracleStockReadbackIsStrict(t *testing.T) {
	intHolder := func(n int64) *cffi.CFFIValueHolder {
		return &cffi.CFFIValueHolder{Value: &cffi.CFFIValueHolder_IntValue{IntValue: n}}
	}
	dupMap := &cffi.CFFIValueHolder{Value: &cffi.CFFIValueHolder_MapValue{MapValue: &cffi.CFFIValueMap{
		Entries: []*cffi.CFFIMapEntry{
			{Key: "k", Value: intHolder(1)},
			{Key: "k", Value: intHolder(2)},
		},
	}}}
	var sites []soStockSite
	if _, _, err := soReadStockValue(dupMap, "$", &sites); err == nil {
		t.Error("a map with a DUPLICATE key was accepted; the emitted document would depend on which " +
			"entry the walk recorded last")
	}
	dupField := &cffi.CFFIValueHolder{Value: &cffi.CFFIValueHolder_ClassValue{ClassValue: &cffi.CFFIValueClass{
		Name: &cffi.CFFITypeName{Namespace: cffi.CFFITypeNamespace_TYPES, Name: "Dup"},
		Fields: []*cffi.CFFIMapEntry{
			{Key: "f", Value: intHolder(1)},
			{Key: "f", Value: intHolder(2)},
		},
	}}}
	if _, _, err := soReadStockValue(dupField, "$", &sites); err == nil {
		t.Error("a class with a DUPLICATE field was accepted")
	}
	// CONTROL: the same shapes with distinct keys are read, so the refusals are
	// about duplication rather than about maps and classes.
	okMap := &cffi.CFFIValueHolder{Value: &cffi.CFFIValueHolder_MapValue{MapValue: &cffi.CFFIValueMap{
		Entries: []*cffi.CFFIMapEntry{
			{Key: "b", Value: intHolder(1)},
			{Key: "a", Value: intHolder(2)},
		},
	}}}
	id, doc, err := soReadStockValue(okMap, "$", &sites)
	if err != nil {
		t.Fatalf("the distinct-key control was refused: %v", err)
	}
	// And the INPUT order is kept, not sorted.
	if id != "map{b=int:1,a=int:2}" || string(doc) != `{"b":1,"a":2}` {
		t.Fatalf("the readback did not preserve entry order: %s / %s", id, doc)
	}
	// An unmodelled CFFI variant is refused rather than rendered as a placeholder
	// that would compare equal to itself.
	if _, _, err := soReadStockValue(&cffi.CFFIValueHolder{}, "$", &sites); err == nil {
		t.Error("an empty/unmodelled value holder was accepted")
	}
}

// TestServingOracleTypeMapCoversTheCorpus proves the registration derived from the
// corpus reaches every name baml_go can ask for on the value side.
//
// A missing CHECKED_TYPES key is not a test failure but a PANIC on the CFFI
// callback thread — a dead process with no attribution — so the coverage is
// asserted up front rather than discovered by a crash.
func TestServingOracleTypeMapCoversTheCorpus(t *testing.T) {
	soEnsureRuntime(t)
	if len(soRuntimeTypes) == 0 {
		t.Fatal("the type map is empty; every registration claim below would be vacuous")
	}
	classes, checked := 0, 0
	checkedSeen := map[string]bool{}
	for name := range soRuntimeTypes {
		switch {
		case strings.HasPrefix(name, "TYPES."):
			classes++
		case strings.HasPrefix(name, "CHECKED_TYPES."):
			checked++
		default:
			t.Errorf("the type map carries the unexpected namespace %q", name)
		}
	}
	if classes == 0 || checked == 0 {
		t.Fatalf("the type map has %d TYPES and %d CHECKED_TYPES entries; both must be non-empty",
			classes, checked)
	}
	for _, f := range servingOracleFixtures {
		for _, c := range f.Bundle.Classes {
			if _, ok := soRuntimeTypes["TYPES."+c.Name.Name]; !ok {
				t.Errorf("%s: class %q is not registered, so stock would decode it dynamically and its raw "+
					"tree (with its ordered check collection) would be lost", f.Name, c.Name.Name)
			}
		}
		for _, e := range f.Bundle.Enums {
			if _, ok := soRuntimeTypes["TYPES."+e.Name.Name]; !ok {
				t.Errorf("%s: enum %q is not registered", f.Name, e.Name.Name)
			}
		}
		// EVERY constrained node needs its checked name, not only the ones at the
		// root: a root list or map hands its elements to serde, and baml_go asks for
		// the COMPOUND name there (List__int, Map__string_int, Optional__int), which
		// only the per-declaration walk supplies.
		want := func(ty schema.Type) {
			if len(ty.Meta.Constraints) == 0 {
				return
			}
			key := "CHECKED_TYPES." + soCheckedValueName(ty)
			if _, ok := soRuntimeTypes[key]; !ok {
				t.Errorf("%s: %q is not registered; baml_go PANICS on the callback thread for a missing "+
					"checked type, which kills the process rather than failing a test", f.Name, key)
			}
			checkedSeen[key] = true
		}
		soWalkTypes(f.Bundle.Target, want)
		for _, c := range f.Bundle.Classes {
			for _, fld := range c.Fields {
				soWalkTypes(fld.Type, want)
			}
		}
	}
	// The compound names are what the per-declaration walk exists for; requiring at
	// least one keeps this from passing on the fixed scalar vocabulary alone.
	compound := 0
	for key := range checkedSeen {
		if strings.Contains(key, "__") {
			compound++
		}
	}
	if compound == 0 {
		t.Error("no constrained node needed a COMPOUND checked name (List__/Map__/Optional__), so the " +
			"per-declaration registration walk is unexercised by the corpus")
	}
	t.Logf("type map: %d TYPES entries, %d CHECKED_TYPES entries; %d constrained nodes needed a name, "+
		"%d of them compound", classes, checked, len(checkedSeen), compound)
}
