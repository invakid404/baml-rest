package bamlutils

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
)

func TestNewOrderedMap_PreservesEntryOrder(t *testing.T) {
	m, err := NewOrderedMap(
		OrderedKV("delta", 1),
		OrderedKV("alpha", 2),
		OrderedKV("charlie", 3),
	)
	if err != nil {
		t.Fatalf("NewOrderedMap: %v", err)
	}
	want := []string{"delta", "alpha", "charlie"}
	if got := m.Keys(); !reflect.DeepEqual(got, want) {
		t.Errorf("Keys: got %v want %v", got, want)
	}
}

func TestNewOrderedMap_RejectsDuplicates(t *testing.T) {
	_, err := NewOrderedMap(
		OrderedKV("a", 1),
		OrderedKV("a", 2),
	)
	if err == nil {
		t.Fatalf("expected duplicate error, got nil")
	}
	if !errors.Is(err, ErrOrderedMapDuplicateKey) {
		t.Errorf("errors.Is(_, ErrOrderedMapDuplicateKey) = false; err=%v", err)
	}
	var keyErr *OrderedMapKeyError
	if !errors.As(err, &keyErr) {
		t.Fatalf("expected *OrderedMapKeyError; got %T", err)
	}
	if keyErr.Key != "a" {
		t.Errorf("Key: got %q want %q", keyErr.Key, "a")
	}
}

func TestMustOrderedMap_PanicsOnDuplicate(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on duplicate")
		}
	}()
	_ = MustOrderedMap(
		OrderedKV("a", 1),
		OrderedKV("a", 2),
	)
}

func TestOrderedMap_Set_AppendsAndRejectsDuplicate(t *testing.T) {
	var m OrderedMap[int]
	if err := m.Set("a", 1); err != nil {
		t.Fatalf("Set a: %v", err)
	}
	if err := m.Set("b", 2); err != nil {
		t.Fatalf("Set b: %v", err)
	}
	if err := m.Set("a", 99); err == nil {
		t.Fatalf("expected duplicate error on re-Set")
	} else if !errors.Is(err, ErrOrderedMapDuplicateKey) {
		t.Errorf("not duplicate sentinel: %v", err)
	}
	if got, _ := m.Get("a"); got != 1 {
		t.Errorf("value mutated after rejected Set: got %d want 1", got)
	}
	if !reflect.DeepEqual(m.Keys(), []string{"a", "b"}) {
		t.Errorf("order mutated after rejected Set: %v", m.Keys())
	}
}

func TestOrderedMap_Replace(t *testing.T) {
	m := MustOrderedMap(OrderedKV("a", 1), OrderedKV("b", 2))
	if err := m.Replace("a", 99); err != nil {
		t.Fatalf("Replace a: %v", err)
	}
	if got, _ := m.Get("a"); got != 99 {
		t.Errorf("Get(a) after Replace: got %d want 99", got)
	}
	if !reflect.DeepEqual(m.Keys(), []string{"a", "b"}) {
		t.Errorf("Replace changed order: %v", m.Keys())
	}
	err := m.Replace("missing", 1)
	if err == nil {
		t.Fatalf("expected missing-key error")
	}
	if !errors.Is(err, ErrOrderedMapMissingKey) {
		t.Errorf("errors.Is(_, ErrOrderedMapMissingKey) = false; err=%v", err)
	}
}

func TestOrderedMap_SetFront(t *testing.T) {
	var m OrderedMap[int]
	if err := m.Set("a", 1); err != nil {
		t.Fatal(err)
	}
	if err := m.SetFront("front", 99); err != nil {
		t.Fatalf("SetFront: %v", err)
	}
	if !reflect.DeepEqual(m.Keys(), []string{"front", "a"}) {
		t.Errorf("SetFront order: %v", m.Keys())
	}
	if err := m.SetFront("front", 0); err == nil {
		t.Fatalf("expected duplicate error on SetFront")
	} else if !errors.Is(err, ErrOrderedMapDuplicateKey) {
		t.Errorf("not duplicate sentinel: %v", err)
	}
}

func TestOrderedMap_Delete(t *testing.T) {
	m := MustOrderedMap(OrderedKV("a", 1), OrderedKV("b", 2), OrderedKV("c", 3))
	if !m.Delete("b") {
		t.Fatalf("Delete b: returned false")
	}
	if !reflect.DeepEqual(m.Keys(), []string{"a", "c"}) {
		t.Errorf("post-Delete order: %v", m.Keys())
	}
	if _, ok := m.Get("b"); ok {
		t.Errorf("Get(b) after Delete should be missing")
	}
	if m.Delete("missing") {
		t.Errorf("Delete missing returned true")
	}
}

func TestOrderedMap_KeysEntriesAreCopies(t *testing.T) {
	m := MustOrderedMap(OrderedKV("a", 1), OrderedKV("b", 2))
	keys := m.Keys()
	keys[0] = "mutated"
	if m.Keys()[0] != "a" {
		t.Errorf("Keys() must return a copy")
	}
	entries := m.Entries()
	entries[0].Key = "mutated"
	if m.Keys()[0] != "a" {
		t.Errorf("Entries() must return a copy")
	}
}

func TestOrderedMap_RangeAndAll(t *testing.T) {
	m := MustOrderedMap(OrderedKV("a", 1), OrderedKV("b", 2), OrderedKV("c", 3))
	var got []string
	m.Range(func(k string, v int) bool {
		got = append(got, k)
		return k != "b"
	})
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("Range early-stop: got %v", got)
	}

	got = got[:0]
	for k := range m.All() {
		got = append(got, k)
	}
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("All iter order: got %v", got)
	}
}

func TestOrderedMap_Clone(t *testing.T) {
	m := MustOrderedMap(OrderedKV("a", 1), OrderedKV("b", 2))
	c := m.Clone()
	if err := c.Set("c", 3); err != nil {
		t.Fatal(err)
	}
	if m.Has("c") {
		t.Errorf("Clone leaked back to original")
	}
}

func TestOrderedMap_IsZero(t *testing.T) {
	var m OrderedMap[int]
	if !m.IsZero() {
		t.Errorf("zero value: IsZero must be true")
	}
	_ = m.Set("a", 1)
	if m.IsZero() {
		t.Errorf("after Set: IsZero must be false")
	}
	m.Clear()
	if !m.IsZero() {
		t.Errorf("after Clear: IsZero must be true")
	}
}

func TestOrderedMap_MarshalJSON_PreservesOrder(t *testing.T) {
	m := MustOrderedMap(
		OrderedKV("delta", 1),
		OrderedKV("alpha", 2),
		OrderedKV("charlie", 3),
	)
	data, err := sonic.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"delta":1,"alpha":2,"charlie":3}`
	if string(data) != want {
		t.Errorf("Marshal:\ngot  %s\nwant %s", string(data), want)
	}
}

func TestOrderedMap_MarshalJSON_Empty(t *testing.T) {
	var m OrderedMap[int]
	data, err := sonic.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(data) != "{}" {
		t.Errorf("empty Marshal: got %s want {}", string(data))
	}
}

func TestOrderedMap_UnmarshalJSON_PreservesOrder(t *testing.T) {
	var m OrderedMap[int]
	input := []byte(`{"delta":1,"alpha":2,"charlie":3,"bravo":4}`)
	if err := sonic.Unmarshal(input, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	want := []string{"delta", "alpha", "charlie", "bravo"}
	if got := m.Keys(); !reflect.DeepEqual(got, want) {
		t.Errorf("Keys: got %v want %v", got, want)
	}
}

func TestOrderedMap_UnmarshalJSON_RejectsDuplicate(t *testing.T) {
	var m OrderedMap[int]
	err := sonic.Unmarshal([]byte(`{"a":1,"a":2}`), &m)
	if err == nil {
		t.Fatalf("expected duplicate error")
	}
	if !errors.Is(err, ErrOrderedMapDuplicateKey) {
		t.Errorf("errors.Is(_, ErrOrderedMapDuplicateKey) = false; err=%v", err)
	}
}

func TestOrderedMap_UnmarshalJSON_NullResetsToZero(t *testing.T) {
	m := MustOrderedMap(OrderedKV("a", 1))
	if err := sonic.Unmarshal([]byte(`null`), &m); err != nil {
		t.Fatalf("Unmarshal null: %v", err)
	}
	if !m.IsZero() {
		t.Errorf("null must reset to zero value; got keys=%v", m.Keys())
	}
}

func TestOrderedMap_UnmarshalJSON_EmptyObject(t *testing.T) {
	var m OrderedMap[int]
	if err := sonic.Unmarshal([]byte(`{}`), &m); err != nil {
		t.Fatalf("Unmarshal {}: %v", err)
	}
	if !m.IsZero() {
		t.Errorf("empty object: IsZero must be true")
	}
	data, err := sonic.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{}" {
		t.Errorf("empty round-trip: got %s want {}", string(data))
	}
}

func TestOrderedMap_UnmarshalJSON_SingleEntry(t *testing.T) {
	var m OrderedMap[string]
	if err := sonic.Unmarshal([]byte(`{"only":"value"}`), &m); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Keys(), []string{"only"}) {
		t.Errorf("single-entry keys: %v", m.Keys())
	}
	v, ok := m.Get("only")
	if !ok || v != "value" {
		t.Errorf("Get(only): got (%q,%v)", v, ok)
	}
}

func TestOrderedMap_ZeroValueIsUsable(t *testing.T) {
	var m OrderedMap[int]
	if err := m.Set("a", 1); err != nil {
		t.Fatalf("zero-value Set: %v", err)
	}
	if m.Len() != 1 {
		t.Errorf("Len: %d", m.Len())
	}
}

func TestOrderedMap_MixedValueRoundtrip(t *testing.T) {
	type Inner struct {
		Name string `json:"name"`
	}
	m := MustOrderedMap(
		OrderedKV("first", &Inner{Name: "one"}),
		OrderedKV("second", &Inner{Name: "two"}),
	)
	data, err := sonic.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"first":{"name":"one"},"second":{"name":"two"}}`
	if string(data) != want {
		t.Errorf("Marshal:\ngot  %s\nwant %s", string(data), want)
	}

	var rt OrderedMap[*Inner]
	if err := sonic.Unmarshal(data, &rt); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(rt.Keys(), []string{"first", "second"}) {
		t.Errorf("Keys: %v", rt.Keys())
	}
	v, _ := rt.Get("second")
	if v.Name != "two" {
		t.Errorf("Get(second).Name: got %q", v.Name)
	}
}

// TestUnmarshalOrderedMap_PathQualifiedDuplicateSentinel pins F1: the
// path-qualified duplicate-key branch must still surface
// ErrOrderedMapDuplicateKey under errors.Is, so schema-level decoders
// using the unexported helper retain the sentinel chain. Previously the
// branch rendered a plain fmt.Errorf with no wrapping and callers lost
// the ability to type-assert on the sentinel.
func TestUnmarshalOrderedMap_PathQualifiedDuplicateSentinel(t *testing.T) {
	_, err := unmarshalOrderedMap[int]([]byte(`{"a":1,"a":2}`), "output_schema.properties")
	if err == nil {
		t.Fatalf("expected duplicate-key error, got nil")
	}
	if !errors.Is(err, ErrOrderedMapDuplicateKey) {
		t.Errorf("errors.Is(_, ErrOrderedMapDuplicateKey) = false; err=%v", err)
	}
	var keyErr *OrderedMapKeyError
	if !errors.As(err, &keyErr) {
		t.Errorf("errors.As(_, *OrderedMapKeyError) = false; err=%v", err)
	} else if keyErr.Key != "a" {
		t.Errorf("OrderedMapKeyError.Key: got %q want %q", keyErr.Key, "a")
	}
	// Existing path-qualified prose remains so callers asserting via
	// strings.Contains keep matching.
	if !strings.Contains(err.Error(), `output_schema.properties: duplicate key "a"`) {
		t.Errorf("missing path-qualified prose; err=%q", err.Error())
	}
}

// TestUnmarshalOrderedMap_RejectsTrailingBytes pins F2: input like
// `{"a":1} garbage` is silently accepted by json.Decoder's default
// behaviour because nothing reads past the closing brace. We explicitly
// pull the next token and require io.EOF so post-object data is
// rejected, matching the strict-input principle of the rest of the
// dynamic-schema decode path.
//
// goccy/go-json's outer Unmarshal layer also rejects trailing top-level
// bytes (with its own "after top-level value" wording) when this decode
// is reached through sonic.Unmarshal(&m). We exercise the helper
// directly so the strict-input contract is pinned at the function
// boundary regardless of whether the outer encoder enforces it too.
func TestUnmarshalOrderedMap_RejectsTrailingBytes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"trailing junk word", `{"a":1} garbage`},
		{"trailing object", `{"a":1}{"b":2}`},
		{"trailing array", `{"a":1}[1,2]`},
		{"trailing number", `{"a":1}42`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := unmarshalOrderedMap[int]([]byte(tc.body), "")
			if err == nil {
				t.Fatalf("expected trailing-data rejection; got nil")
			}
			if !strings.Contains(err.Error(), "trailing") {
				t.Errorf("error does not mention trailing data: %q", err.Error())
			}
		})
	}
}

// TestUnmarshalOrderedMap_RejectsTrailingBytes_PathQualified covers the
// same F2 contract through the path-qualified branch used by
// DynamicOutputSchema.UnmarshalJSON / DynamicClass.UnmarshalJSON. The
// path prefix must appear in the error so existing field-qualified
// diagnostics remain readable.
func TestUnmarshalOrderedMap_RejectsTrailingBytes_PathQualified(t *testing.T) {
	_, err := unmarshalOrderedMap[int]([]byte(`{"a":1} garbage`), "output_schema.properties")
	if err == nil {
		t.Fatalf("expected trailing-data rejection; got nil")
	}
	if !strings.Contains(err.Error(), "output_schema.properties") {
		t.Errorf("missing path prefix: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "trailing") {
		t.Errorf("error does not mention trailing data: %q", err.Error())
	}
}

// TestUnmarshalOrderedMap_AcceptsTrailingWhitespace pins the inverse
// contract of the trailing-bytes rejection: trailing whitespace after
// the top-level object is accepted, matching how json.Decoder treats
// whitespace as insignificant. Without this, callers feeding pretty-
// printed JSON with a final newline would suddenly start failing.
func TestUnmarshalOrderedMap_AcceptsTrailingWhitespace(t *testing.T) {
	for _, body := range []string{
		`{"a":1}`,
		`{"a":1} `,
		"{\"a\":1}\n",
		"{\"a\":1}\r\n\t  ",
	} {
		var m OrderedMap[int]
		if err := sonic.Unmarshal([]byte(body), &m); err != nil {
			t.Errorf("unexpected error for %q: %v", body, err)
		}
	}
}
