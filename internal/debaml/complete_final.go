package debaml

// completeUnclosedFinal reproduces BAML's non-stream Parse EOF recovery of an
// unclosed JSON structure, for the NATIVE-ONLY streaming FINAL closure only (a
// completed [DONE] stream whose accumulated model text lost its closing bracket
// is an ordinary success for BAML, whose stream final calls the non-stream Parse).
//
// It returns (completed, true) when s had an unclosed object/array — the completed
// text is exactly the equivalent CLOSED input, so feeding it back through the SAME
// extract/fix/coerce machinery yields byte-identical output to native's coercion of
// a naturally-closed object (and, by construction, to BAML's non-stream Parse). It
// returns (s, false) when nothing was unclosed.
//
// BAML's EOF close, reproduced here as a single string→string pass:
//   - a trailing partial STRING value / array element is RECOVERED (its closing
//     quote is appended: {"name":"Ad -> name "Ad"). Both double- AND single-quoted
//     strings are tracked, closing with the delimiter that opened them ({'name':'Ad
//     -> {'name':'Ad'}, which the fixer then normalises); a dangling backslash on a
//     recovered double-quoted string is preserved as a literal backslash ({"n":"a\
//     -> {"n":"a\\"}) rather than escaping the appended quote into invalid JSON;
//   - a trailing partial/complete KEY with no value, or a dangling ':' , is DROPPED
//     back to the last complete field so the absent field takes its coercion default
//     (list -> [], map -> {}) or errors as a missing required scalar; this covers a
//     quoted key ({'name -> {), a JSONish UNQUOTED key ({name -> {, {name: -> {), and
//     a single-quoted key ({'name' -> {);
//   - a trailing partial NUMBER trims incomplete-number chars (3. -> 3, 3e -> 3,
//     3.1e -> 3.1); a punct-only token ("-") has no digit and drops the field;
//   - every open container is CLOSED with the complete content parsed so far.
//
// Applied ONLY behind SupportsNativeFinal (admitted schemas), so shapes the admitted
// matrix excludes — e.g. a class with a non-last unquoted scalar, whose compact
// unclosed form BAML tokenizes differently — never reach it. LIVE-CAPTURED byte-exact
// vs BAML v0.223 over a byte-by-byte prefix sweep of every admitted last-field type
// (int/float/bool/string/list/map/enum/nested-class/negative), see
// integration.TestStream7CUnclosedFinalParity.
func completeUnclosedFinal(s string) (string, bool) {
	type frame struct {
		arr         bool
		safeLen     int  // output length at the last safely-closeable point of this frame
		expectValue bool // object: saw ':' (next string is a value, not a key)
		pendingKey  bool // object: a key string closed but no ':' yet
	}
	out := make([]byte, 0, len(s)+8)
	var fr []frame
	top := func() *frame { return &fr[len(fr)-1] }
	inStr, esc, strIsKey := false, false, false
	var strQuote byte   // active string delimiter while inStr: '"' or '\''
	trailTokStart := -1 // output index of a trailing unquoted token that ran to EOF
	commit := func() {  // the innermost frame's content up to here is complete
		if len(fr) > 0 {
			top().safeLen = len(out)
		}
	}
	i := 0
	for i < len(s) {
		c := s[i]
		if inStr {
			out = append(out, c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == strQuote:
				inStr = false
				if len(fr) > 0 {
					if strIsKey {
						top().pendingKey = true // key closed; awaiting ':'
					} else {
						commit() // a value/element string completed
					}
				}
			}
			i++
			continue
		}
		switch c {
		case '"', '\'':
			// A quoted string opens. `'` reaches this switch only at a fresh token
			// START (a `'` mid-bareword like `here's` is consumed by the default
			// token loop below), so single quotes need no extra structural gating.
			inStr, esc, strQuote = true, false, c
			strIsKey = len(fr) > 0 && !top().arr && !top().expectValue
			if len(fr) > 0 && !top().arr {
				top().expectValue = false
			}
			out = append(out, c)
			i++
		case '{', '[':
			fr = append(fr, frame{arr: c == '[', safeLen: len(out) + 1})
			out = append(out, c)
			i++
		case '}', ']':
			out = append(out, c)
			i++
			if len(fr) > 0 {
				fr = fr[:len(fr)-1]
			}
			commit() // the closed container is a complete value in its parent
		case ':':
			if len(fr) > 0 {
				top().expectValue = true
				top().pendingKey = false
			}
			out = append(out, c)
			i++
		case ',':
			if len(fr) > 0 {
				top().expectValue = false
				top().pendingKey = false
			}
			out = append(out, c)
			i++
		case ' ', '\t', '\n', '\r':
			out = append(out, c)
			i++
		default:
			// An unquoted token. In an object KEY position (not an array element, not
			// after ':') it is a JSONish unquoted key, which ends at ':' — recognised
			// like a closed quoted key (pendingKey) so a dangling `{name` / `{name:`
			// drops the valueless field at EOF exactly as BAML defaults it. Otherwise
			// it is a scalar VALUE (number / true / false / null / bareword): consume to
			// the next delimiter; a complete token at EOF is a committed value.
			inKeyPos := len(fr) > 0 && !top().arr && !top().expectValue
			start := i
			for i < len(s) {
				d := s[i]
				if d == ',' || d == '}' || d == ']' || d == ' ' || d == '\t' || d == '\n' || d == '\r' {
					break
				}
				if inKeyPos && d == ':' {
					break // an unquoted key ends at its ':'
				}
				i++
			}
			tokOut := len(out)
			out = append(out, s[start:i]...)
			if inKeyPos {
				top().pendingKey = true // unquoted key; awaiting ':' (dropped valueless at EOF)
			} else {
				if len(fr) > 0 {
					top().expectValue = false
				}
				if i >= len(s) {
					trailTokStart = tokOut // value token ran to EOF: may be a partial number
				} else if len(fr) > 0 {
					commit()
				}
			}
		}
	}
	if len(fr) == 0 {
		return s, false
	}
	switch {
	case inStr:
		if strIsKey {
			out = out[:top().safeLen] // drop the partial key
		} else {
			if esc {
				// The string ended on a lone backslash. BAML keeps it as a LITERAL
				// backslash ({"n":"a\ -> {"n":"a\\"}); appending only the closing quote
				// here would let that backslash escape the quote and yield invalid JSON,
				// so complete the escape pair first. (A single-quoted string carrying a
				// backslash is declined downstream by the fixer — the #583 deferral — so
				// this pairing only needs to keep the double-quoted recovery valid.)
				out = append(out, '\\')
			}
			out = append(out, strQuote) // recover the trailing value/element string
			commit()
		}
	case trailTokStart >= 0:
		// A trailing unquoted token that ran to EOF. If it is number-like (contains a
		// digit), trim trailing incomplete-number chars; a punct-only token has no
		// digit and drops the field; a bareword (true/false/null/partial) is kept.
		tok := out[trailTokStart:]
		hasDigit := false
		for _, b := range tok {
			if b >= '0' && b <= '9' {
				hasDigit = true
				break
			}
		}
		if hasDigit {
			end := len(tok)
			for end > 0 {
				b := tok[end-1]
				if b == '.' || b == 'e' || b == 'E' || b == '+' || b == '-' {
					end--
					continue
				}
				break
			}
			out = out[:trailTokStart+end]
			commit()
		} else {
			punctOnly := true
			for _, b := range tok {
				if !(b == '.' || b == 'e' || b == 'E' || b == '+' || b == '-') {
					punctOnly = false
					break
				}
			}
			if punctOnly {
				out = out[:top().safeLen]
			} else {
				commit()
			}
		}
	case !top().arr && (top().expectValue || top().pendingKey):
		// dangling ':' OR a complete key with no value: drop the valueless field.
		out = out[:top().safeLen]
	}
	// Close the open frames with the complete content parsed so far (BAML's jsonish
	// EOF close), yielding exactly the equivalent closed input.
	for len(fr) > 0 {
		if top().arr {
			out = append(out, ']')
		} else {
			out = append(out, '}')
		}
		fr = fr[:len(fr)-1]
	}
	return string(out), true
}
