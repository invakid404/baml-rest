package admission

// Adversarial coverage for fixShortEscapes — the backslash-parity-aware rewrite
// of sonic's two divergent long escapes to the short forms BAML emits. The whole
// risk is misreading an escaped-backslash pair, so every input is built from
// explicit bytes (no source-level escape sequences) and the parity edge cases
// (a literal "u0008" behind an even/odd backslash run) are pinned directly.

import (
	"bytes"
	"testing"
)

func TestFixShortEscapes(t *testing.T) {
	// bytes for the two long forms sonic emits and the short forms BAML emits.
	bs := byte('\\')
	longBS := []byte{bs, 'u', '0', '0', '0', '8'} // 0x08
	longFF := []byte{bs, 'u', '0', '0', '0', 'c'} //
	shortBS := []byte{bs, 'b'}                    // \b
	shortFF := []byte{bs, 'f'}                    // \f

	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"empty", []byte{}, []byte{}},
		{"no_escapes", []byte(`{"k":"plain value"}`), []byte(`{"k":"plain value"}`)},
		{"genuine_backspace", longBS, shortBS},
		{"genuine_formfeed", longFF, shortFF},
		{"both_genuine", cat(longBS, []byte("mid"), longFF), cat(shortBS, []byte("mid"), shortFF)},

		// Escaped-backslash parity: the leading backslash is the payload of a "\\"
		// pair, so "u0008" is literal text and must NOT be rewritten.
		{"escaped_backslash_then_u0008", []byte{bs, bs, 'u', '0', '0', '0', '8'}, []byte{bs, bs, 'u', '0', '0', '0', '8'}},
		{"escaped_backslash_then_u000c", []byte{bs, bs, 'u', '0', '0', '0', 'c'}, []byte{bs, bs, 'u', '0', '0', '0', 'c'}},
		{"lone_escaped_backslash", []byte{bs, bs}, []byte{bs, bs}},

		// Odd backslash run: "\\" (literal backslash) followed by a GENUINE 0x08.
		{"escaped_backslash_then_genuine_backspace",
			[]byte{bs, bs, bs, 'u', '0', '0', '0', '8'},
			[]byte{bs, bs, bs, 'b'}},
		// Even run then literal: two "\\" pairs then literal "u0008" — unchanged.
		{"two_escaped_backslashes_then_literal",
			[]byte{bs, bs, bs, bs, 'u', '0', '0', '0', '8'},
			[]byte{bs, bs, bs, bs, 'u', '0', '0', '0', '8'}},
		// Odd longer run: two "\\" pairs + one literal backslash then GENUINE 0x08.
		{"two_escaped_backslashes_then_genuine_backspace",
			[]byte{bs, bs, bs, bs, bs, 'u', '0', '0', '0', '8'},
			[]byte{bs, bs, bs, bs, bs, 'b'}},

		// Non-diverging escapes are preserved untouched.
		{"other_u_escape_preserved", []byte{bs, 'u', '0', '0', '0', '1'}, []byte{bs, 'u', '0', '0', '0', '1'}},
		{"short_escapes_preserved", []byte{bs, 'n', bs, 't', bs, 'r', bs, '"'}, []byte{bs, 'n', bs, 't', bs, 'r', bs, '"'}},

		// A realistic JSON fragment with a genuine backspace inside a string value.
		{"json_fragment",
			cat([]byte(`{"text":"a`), longBS, []byte(`b"}`)),
			cat([]byte(`{"text":"a`), shortBS, []byte(`b"}`))},

		// Both a genuine short byte AND a literal escaped-backslash "u0008" in one
		// string: rewrite the genuine one, leave the literal one alone.
		{"genuine_and_literal_together",
			cat(longBS, []byte("X"), []byte{bs, bs}, []byte("u0008Y"), longFF),
			cat(shortBS, []byte("X"), []byte{bs, bs}, []byte("u0008Y"), shortFF)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fixShortEscapes(append([]byte(nil), tc.in...))
			if !bytes.Equal(got, tc.want) {
				t.Errorf("fixShortEscapes(%q) = %q, want %q", bodyDigest(tc.in), bodyDigest(got), bodyDigest(tc.want))
			}
		})
	}
}

// TestFixShortEscapesEndToEnd runs a string carrying real 0x08/0x0c bytes AND a
// literal backslash-then-u0008 sequence through the SHIPPED marshaler, and pins
// the exact JSON bytes: genuine control bytes become short escapes; the escaped
// backslash and its literal "u0008" survive; the backslash itself is doubled.
func TestFixShortEscapesEndToEnd(t *testing.T) {
	bs := byte('\\')
	// input string: A <0x08> B \ u 0 0 0 8 C <0x0c> D
	in := string([]byte{'A', 0x08, 'B', bs, 'u', '0', '0', '0', '8', 'C', 0x0c, 'D'})

	got, err := canonicalSonicMarshaler(in)
	if err != nil {
		t.Fatalf("canonicalSonicMarshaler: %v", err)
	}
	// Expected JSON string literal: "A\bB\\u0008C\fD"
	want := []byte{
		'"',
		'A', bs, 'b', 'B',
		bs, bs, 'u', '0', '0', '0', '8',
		'C', bs, 'f', 'D',
		'"',
	}
	if !bytes.Equal(got, want) {
		t.Errorf("shipped marshaler output = %q, want %q", bodyDigest(got), bodyDigest(want))
	}
}
