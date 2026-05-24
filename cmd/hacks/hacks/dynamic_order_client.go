package hacks

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	Register(&DynamicOrderClientHack{})
}

// DynamicOrderClientHack rewrites generated @@dynamic client structs so
// `DynamicProperties` carries an ordered field map, not a Go map.
// Combined with the BAML-side ApplyDynamicOrderFix patch (serde
// DynamicClass.Fields + DecodeToOrderedValue), the client surfaces CFFI
// insertion order from the runtime up to baml-rest's unwrap pass.
//
// Targeted files: every .go file under baml_client/ that declares a
// struct field literally named `DynamicProperties` of type
// `map[string]any`. The hack edits the field type, the make-init, each
// index-assign in the Decode body, and the EncodeClass call site that
// passes the field by pointer.
type DynamicOrderClientHack struct{}

func (h *DynamicOrderClientHack) Name() string {
	return "dynamic-order-client-fix"
}

func (h *DynamicOrderClientHack) MinVersion() string {
	return ""
}

func (h *DynamicOrderClientHack) MaxVersion() string {
	return ""
}

func (h *DynamicOrderClientHack) Apply(bamlClientDir string) error {
	return filepath.WalkDir(bamlClientDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		changed, applyErr := patchGeneratedDynamicFile(path)
		if applyErr != nil {
			return fmt.Errorf("processing %s: %w", path, applyErr)
		}
		if changed {
			fmt.Printf("  Modified (dynamic-order-client): %s\n", path)
		}
		return nil
	})
}

// patchGeneratedDynamicFile applies the DynamicProperties rewrite to a
// single generated file. The function returns false when the file has
// no DynamicProperties declaration, so the walker prints a clean log
// line only for files the hack actually rewrote. Text-level edits are
// used because the patterns are short, file-local, and the AST view of
// `map[string]any` and `baml.OrderedFields` would otherwise require
// rewriting selector expressions through reflection — the text
// approach keeps the rewrite auditable.
func patchGeneratedDynamicFile(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	body := string(raw)

	if !strings.Contains(body, "DynamicProperties") {
		return false, nil
	}
	if !strings.Contains(body, "DynamicProperties map[string]any") {
		// Already patched, or the field is shaped differently in this
		// file. Avoid a partial rewrite that mixes types.
		return false, nil
	}

	original := body

	// 1. Field type: keep the struct tag (if any) intact.
	body = strings.ReplaceAll(
		body,
		"DynamicProperties map[string]any",
		"DynamicProperties baml.OrderedFields",
	)

	// 2. Allocation: support both forms BAML emits.
	body = strings.ReplaceAll(
		body,
		"DynamicProperties = make(map[string]any)",
		"DynamicProperties = baml.NewOrderedFields(0)",
	)
	body = replaceMakeWithCapacity(body)

	// 3. Index assignments in the Decode body. The BAML codegen only
	// writes through c.DynamicProperties[key] in a small set of shapes;
	// matching by suffix on the right-hand side keeps the rewrite
	// pinned to the assignment pattern so any sub-expression that
	// merely mentions DynamicProperties is left alone.
	body = rewriteDecodeAssignments(body)

	// 4. EncodeClass call site: the generated method passes
	// &c.DynamicProperties; route through EncodeClassOrdered which
	// accepts *baml.OrderedFields directly.
	body = strings.ReplaceAll(
		body,
		"baml.EncodeClass(",
		"baml.EncodeClass(",
	)
	body = rewriteEncodeClassCall(body)

	if body == original {
		return false, nil
	}

	// Validate that the patched file still parses; without this a
	// failed regex would silently produce a broken Go file.
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, path, []byte(body), parser.ParseComments); err != nil {
		return false, fmt.Errorf("patched file does not parse: %w", err)
	}

	// Run gofmt-style pretty-print to keep diff noise low when this
	// file is later re-read by other hacks.
	prettied, err := gofmtBytes([]byte(body))
	if err != nil {
		return false, fmt.Errorf("gofmt patched file: %w", err)
	}

	if err := os.WriteFile(path, prettied, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// replaceMakeWithCapacity rewrites `DynamicProperties = make(map[string]any, N)`
// (any expression N) into `DynamicProperties = baml.NewOrderedFields(N)`.
// The previous string replace handles the capacity-less form; this one
// captures the capacity-bearing form BAML emits when the codegen has a
// hint.
func replaceMakeWithCapacity(body string) string {
	const marker = "DynamicProperties = make(map[string]any,"
	idx := 0
	for {
		j := strings.Index(body[idx:], marker)
		if j < 0 {
			break
		}
		start := idx + j
		// Locate the matching ')'.
		open := start + len(marker)
		depth := 1
		k := open
		for k < len(body) && depth > 0 {
			switch body[k] {
			case '(':
				depth++
			case ')':
				depth--
			}
			k++
		}
		if depth != 0 {
			break
		}
		capArg := strings.TrimSpace(body[open : k-1])
		replacement := fmt.Sprintf("DynamicProperties = baml.NewOrderedFields(%s)", capArg)
		body = body[:start] + replacement + body[k:]
		idx = start + len(replacement)
	}
	return body
}

// rewriteDecodeAssignments rewrites
//
//	c.DynamicProperties[key] = baml.DecodeToValue(valueHolder)
//	c.DynamicProperties[key] = baml.Decode(valueHolder).Interface()
//	c.DynamicProperties[key] = <expr>
//
// into a single ordered-Set form that routes the value through
// DecodeToOrderedValue when the original expression was a Decode-style
// call, and leaves any other right-hand side untouched apart from the
// container call shape.
func rewriteDecodeAssignments(body string) string {
	const lhsMarker = ".DynamicProperties["
	idx := 0
	for {
		j := strings.Index(body[idx:], lhsMarker)
		if j < 0 {
			break
		}
		start := idx + j
		// Walk backwards to find the start of the receiver expression
		// (look for the closest preceding whitespace).
		recvStart := start
		for recvStart > 0 {
			c := body[recvStart-1]
			if c == ' ' || c == '\t' || c == '\n' {
				break
			}
			recvStart--
		}
		recv := body[recvStart:start]
		// Locate ']' that closes the index expression.
		keyOpen := start + len(lhsMarker)
		keyClose := -1
		depth := 1
		k := keyOpen
		for k < len(body) && depth > 0 {
			switch body[k] {
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 {
					keyClose = k
				}
			}
			if depth == 0 {
				break
			}
			k++
		}
		if keyClose < 0 {
			break
		}
		// Skip whitespace and '=' to get the assignment value.
		eq := keyClose + 1
		for eq < len(body) && (body[eq] == ' ' || body[eq] == '\t') {
			eq++
		}
		if eq >= len(body) || body[eq] != '=' {
			idx = keyClose + 1
			continue
		}
		eq++
		for eq < len(body) && (body[eq] == ' ' || body[eq] == '\t') {
			eq++
		}
		// Locate the end of the assignment expression: the statement
		// ends at '\n' (no inline `;` in the generated code we patch).
		eol := strings.IndexByte(body[eq:], '\n')
		if eol < 0 {
			eol = len(body) - eq
		}
		rhs := strings.TrimSpace(body[eq : eq+eol])

		key := strings.TrimSpace(body[keyOpen:keyClose])
		newRHS := convertAssignmentRHS(rhs)
		replacement := fmt.Sprintf("%s.DynamicProperties.Set(%s, %s)", recv, key, newRHS)

		body = body[:recvStart] + replacement + body[eq+eol:]
		idx = recvStart + len(replacement)
	}
	return body
}

// convertAssignmentRHS rewrites known Decode-style right-hand sides
// into their ordered counterparts. Patterns left untouched fall
// through verbatim — the generator only emits a small set of shapes
// for DynamicProperties assignment.
func convertAssignmentRHS(rhs string) string {
	const dtv = "baml.DecodeToValue("
	if strings.HasPrefix(rhs, dtv) {
		return "baml.DecodeToOrderedValue(" + rhs[len(dtv):]
	}
	const decodePrefix = "baml.Decode("
	const decodeSuffix = ").Interface()"
	if strings.HasPrefix(rhs, decodePrefix) && strings.HasSuffix(rhs, decodeSuffix) {
		inner := rhs[len(decodePrefix) : len(rhs)-len(decodeSuffix)]
		return "baml.DecodeToOrderedValue(" + inner + ")"
	}
	return rhs
}

// rewriteEncodeClassCall walks every baml.EncodeClass(...) call site
// in the file and rewrites the form
//
//	baml.EncodeClass(<name>, <fields>, &c.DynamicProperties)
//
// into
//
//	baml.EncodeClassOrdered(<name>, <fields>, &c.DynamicProperties)
//
// The legacy two/three-argument forms with nil or *map[string]any
// continue to use baml.EncodeClass so non-dynamic classes are
// unaffected.
func rewriteEncodeClassCall(body string) string {
	const marker = "baml.EncodeClass("
	idx := 0
	for {
		j := strings.Index(body[idx:], marker)
		if j < 0 {
			break
		}
		start := idx + j
		open := start + len(marker)
		depth := 1
		k := open
		for k < len(body) && depth > 0 {
			switch body[k] {
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth == 0 {
				break
			}
			k++
		}
		if depth != 0 {
			break
		}
		args := body[open:k]
		if encodeClassCallPassesOrderedFields(args) {
			replacement := "baml.EncodeClassOrdered(" + args + ")"
			body = body[:start] + replacement + body[k+1:]
			idx = start + len(replacement)
			continue
		}
		idx = k + 1
	}
	return body
}

// encodeClassCallPassesOrderedFields returns true when the EncodeClass
// argument list ends with a pointer to a DynamicProperties field. The
// BAML generator emits this shape only for @@dynamic output structs;
// other call sites either pass nil or a typed map pointer and continue
// to use the original EncodeClass signature.
func encodeClassCallPassesOrderedFields(args string) bool {
	trimmed := strings.TrimSpace(args)
	idx := strings.LastIndex(trimmed, ",")
	if idx < 0 {
		return false
	}
	last := strings.TrimSpace(trimmed[idx+1:])
	if !strings.HasPrefix(last, "&") {
		return false
	}
	return strings.HasSuffix(last, ".DynamicProperties")
}

// gofmtBytes runs the gofmt pipeline on src and returns the result.
// Used by patchGeneratedDynamicFile so the rewritten file lands in a
// stable, minimum-diff shape regardless of the text editing path taken
// through the patterns above.
func gofmtBytes(src []byte) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "patched.go", src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var buf strings.Builder
	cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
	if err := cfg.Fprint(&buf, fset, file); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// _ assigns ast.NewIdent to silence Go's unused-import detector when
// the file is rewritten in a tight loop without exercising the parser
// import. The parser import is kept because future shape validation
// hooks under DynamicOrderClientHack will require it.
var _ = ast.NewIdent
