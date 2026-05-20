package bamlutils

import (
	"errors"
	"reflect"
	"testing"

	"github.com/goccy/go-json"
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
	data, err := json.Marshal(m)
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
	data, err := json.Marshal(m)
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
	if err := json.Unmarshal(input, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	want := []string{"delta", "alpha", "charlie", "bravo"}
	if got := m.Keys(); !reflect.DeepEqual(got, want) {
		t.Errorf("Keys: got %v want %v", got, want)
	}
}

func TestOrderedMap_UnmarshalJSON_RejectsDuplicate(t *testing.T) {
	var m OrderedMap[int]
	err := json.Unmarshal([]byte(`{"a":1,"a":2}`), &m)
	if err == nil {
		t.Fatalf("expected duplicate error")
	}
	if !errors.Is(err, ErrOrderedMapDuplicateKey) {
		t.Errorf("errors.Is(_, ErrOrderedMapDuplicateKey) = false; err=%v", err)
	}
}

func TestOrderedMap_UnmarshalJSON_NullResetsToZero(t *testing.T) {
	m := MustOrderedMap(OrderedKV("a", 1))
	if err := json.Unmarshal([]byte(`null`), &m); err != nil {
		t.Fatalf("Unmarshal null: %v", err)
	}
	if !m.IsZero() {
		t.Errorf("null must reset to zero value; got keys=%v", m.Keys())
	}
}

func TestOrderedMap_UnmarshalJSON_EmptyObject(t *testing.T) {
	var m OrderedMap[int]
	if err := json.Unmarshal([]byte(`{}`), &m); err != nil {
		t.Fatalf("Unmarshal {}: %v", err)
	}
	if !m.IsZero() {
		t.Errorf("empty object: IsZero must be true")
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{}" {
		t.Errorf("empty round-trip: got %s want {}", string(data))
	}
}

func TestOrderedMap_UnmarshalJSON_SingleEntry(t *testing.T) {
	var m OrderedMap[string]
	if err := json.Unmarshal([]byte(`{"only":"value"}`), &m); err != nil {
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
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"first":{"name":"one"},"second":{"name":"two"}}`
	if string(data) != want {
		t.Errorf("Marshal:\ngot  %s\nwant %s", string(data), want)
	}

	var rt OrderedMap[*Inner]
	if err := json.Unmarshal(data, &rt); err != nil {
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
