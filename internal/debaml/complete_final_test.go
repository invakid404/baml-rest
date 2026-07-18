package debaml

import "testing"

// TestCompleteUnclosedFinal pins the string→string EOF-completion transform (the
// pure half of the native FINAL EOF recovery; parity vs live BAML is proven in
// integration.TestStream7CUnclosedFinalParity). A closed / balanced input is a no-op.
func TestCompleteUnclosedFinal(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
		changed        bool
	}{
		{"closed noop", `{"name":"Ada","age":36}`, `{"name":"Ada","age":36}`, false},
		{"balanced noop", `{"a":[1,2]}`, `{"a":[1,2]}`, false},
		{"int last", `{"name":"Ada","age":36`, `{"name":"Ada","age":36}`, true},
		// Trailing whitespace/comma are left in place (native's fixer tolerates them);
		// the close bracket is appended after. The PARSED result is validated end-to-end
		// in integration.TestStream7CUnclosedFinalParity.
		{"int last trailing space", `{"name":"Ada","age":36 `, `{"name":"Ada","age":36 }`, true},
		{"int last trailing comma", `{"name":"Ada","age":36,`, `{"name":"Ada","age":36,}`, true},
		{"unterminated value string", `{"a":"x","b":"wor`, `{"a":"x","b":"wor"}`, true},
		{"unterminated array element", `{"t":["a","b`, `{"t":["a","b"]}`, true},
		{"array trailing comma", `{"t":["a","b",`, `{"t":["a","b",]}`, true},
		{"pending key dropped", `{"name":"Ada","tag`, `{"name":"Ada"}`, true},
		{"complete key no colon dropped", `{"name":"Ada","tags"`, `{"name":"Ada"}`, true},
		{"dangling colon dropped", `{"name":"Ada","age":`, `{"name":"Ada"}`, true},
		{"nested object value completed", `{"label":"L","inner":{"x":"v","y":9`, `{"label":"L","inner":{"x":"v","y":9}}`, true},
		{"map value completed", `{"name":"Ada","meta":{"k":"v"`, `{"name":"Ada","meta":{"k":"v"}}`, true},
		{"partial number dot", `{"name":"Ada","score":3.`, `{"name":"Ada","score":3}`, true},
		{"partial number exp", `{"name":"Ada","score":3.1e`, `{"name":"Ada","score":3.1}`, true},
		{"lone minus dropped", `{"name":"Ada","score":-`, `{"name":"Ada"}`, true},
		{"bareword kept", `{"name":"Ada","ok":tru`, `{"name":"Ada","ok":tru}`, true},
		// --- round 13 (CodeRabbit discussion_r3609109500 / _r3609264303): SINGLE-quoted
		// strings, JSONish UNQUOTED keys, and a dangling backslash. The completion emits
		// the equivalent CLOSED JSONish (the fixer normalises single quotes / unquoted
		// keys downstream); end-to-end parity vs live BAML is in
		// integration.TestStream7CUnclosedFinalParity.
		{"single-quoted value recovered", `{'name':'Ad`, `{'name':'Ad'}`, true},
		{"single-quoted value complete, obj unclosed", `{'name':'Ada'`, `{'name':'Ada'}`, true},
		{"single-quoted key no colon dropped", `{'name'`, `{}`, true},
		{"single-quoted key dangling colon dropped", `{'name':`, `{}`, true},
		{"unquoted key no colon dropped", `{name`, `{}`, true},
		{"unquoted key dangling colon dropped", `{name:`, `{}`, true},
		{"unquoted key with complete list value", `{a:["x"]`, `{a:["x"]}`, true},
		{"unquoted key with int value completed", `{age:3`, `{age:3}`, true},
		// A trailing double-quoted string ending on a lone backslash keeps the backslash
		// LITERAL: complete the escape pair so the appended close quote is not escaped into
		// invalid JSON ({"n":"a\ -> {"n":"a\\"}). BAML content is a single backslash.
		{"dangling backslash preserved (double)", `{"name":"Ad\`, `{"name":"Ad\\"}`, true},
		// A single-quoted string carrying a backslash is completed structurally too, but
		// the fixer declines any backslash in a single-quoted string (the #583 deferral),
		// so this stays an under-approximation end-to-end — pinned here for the transform.
		{"dangling backslash preserved (single)", `{'name':'Ad\`, `{'name':'Ad\\'}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, changed := completeUnclosedFinal(c.in)
			if changed != c.changed {
				t.Errorf("changed = %v, want %v", changed, c.changed)
			}
			if got != c.want {
				t.Errorf("completeUnclosedFinal(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
