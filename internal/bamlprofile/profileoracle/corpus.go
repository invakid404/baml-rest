// Package profileoracle is the de-BAML Slice 2 PROFILE differential oracle. It
// proves internal/bamlprofile's get_env engine configuration reproduces BAML
// v0.223.0 byte-for-byte, using an UNTOUCHED stock BAML v0.223.0 runtime (loaded
// via CFFI) as the authority — mirroring internal/nativeprompt/staticoracle.
//
// Two legs render the SAME get_env template corpus:
//
//   - PROFILE leg (pure Go, no CGO): bamlprofile.New(...).Render(...), after
//     reproducing BAML's render_minijinja template preprocessing (dedent + trim,
//     engine/baml-lib/jinja-runtime/src/lib.rs:265-286) so both legs feed get_env
//     the same template. The preprocessing is render-layer, not get_env, so it
//     lives here in the harness, not in the profile.
//   - BAML leg (CGO, `integration` build tag only): a stock v0.223 runtime built
//     in-memory from a generated .baml project whose functions carry the corpus
//     templates as prompts; BuildRequest renders each and builds the provider
//     request (never sent), from which the rendered bytes are read back.
//
// This file (no build tag) holds the corpus, the .baml source generator, and the
// profile leg — all pure-Go and CFFI-free, so `go build ./...` and the default
// `go test` stay CGO-free. The BAML leg and byte-exact comparison live in
// oracle_integration_test.go behind `//go:build integration`.
package profileoracle

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/invakid404/baml-rest/internal/bamlprofile"
)

// BAML role-marker literals (engine/baml-lib/jinja-runtime/src/lib.rs), copied
// here so the oracle's chat split is self-contained and does not reach into the
// profile's unexported constants. They must match bamlprofile/globals.go.
const (
	roleDelimiter   = "BAML_CHAT_ROLE_MAGIC_STRING_DELIMITER"
	roleMarkerStart = ":baml-start-baml:"
	roleMarkerEnd   = ":baml-end-baml:"
)

func roleOfMarker(chunk string) (string, error) {
	inner := strings.TrimSuffix(strings.TrimPrefix(chunk, roleMarkerStart), roleMarkerEnd)
	var raw map[string]any
	if err := json.Unmarshal([]byte(inner), &raw); err != nil {
		return "", fmt.Errorf("profileoracle: parse role marker %q: %w", inner, err)
	}
	role, _ := raw["role"].(string)
	if role == "" {
		return "", fmt.Errorf("profileoracle: role marker missing role: %q", inner)
	}
	return role, nil
}

// clientName is the single stock OpenAI client every corpus function uses. The
// oracle only calls BuildRequest (renders + builds the provider request, never
// sends), so the fake key and `.invalid` base URL are never contacted.
const clientName = "ProfileOracleClient"

// Param is a BAML function parameter (name + BAML type) declared for a row whose
// template reads a variable. Templates that use only literals declare none.
type Param struct {
	Name     string
	BamlType string // e.g. "int", "float", "string", "bool", "int[]", "int?"
}

// Row is one differential case.
type Row struct {
	// ID is the stable identifier (also the basis of the BAML function name).
	ID string
	// Surface groups rows for readability (whitespace, none, filter, ...).
	Surface string
	// Params declares the function's typed parameters; nil when the template uses
	// only literals.
	Params []Param
	// Args are the values bound to Params: the BuildRequest kwargs AND the profile
	// render context. Use int64/float64/string/bool/nil and []any/map[string]any.
	Args map[string]any
	// Template is the raw prompt body, embedded verbatim between #" and "#. Keep
	// it column-0 (no common leading indent) so BAML's dedent is a no-op.
	Template string
	// Chat is true when the template uses _.role/_.chat: the rendered output is a
	// message list, compared message-by-message rather than as one completion.
	Chat bool
}

// FuncName is the BAML function name generated for a row.
func (r Row) FuncName() string {
	var b strings.Builder
	b.WriteString("Row_")
	for _, c := range r.ID {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// blockContent is the exact bytes placed between #" and "# for a row, and the
// exact bytes the profile leg preprocesses. Wrapping the template in newlines
// matches how the generated .baml lays the prompt out; because the template is
// column-0, BAML's dedent (min indent over non-empty lines) is 0 and its trim
// removes only the wrapper newlines — identical on both legs.
func (r Row) blockContent() string {
	return "\n" + r.Template + "\n"
}

// dedentAndTrim reproduces BAML render_minijinja's template preprocessing
// (engine/baml-lib/jinja-runtime/src/lib.rs:265-286): dedent by the minimum
// leading-whitespace of non-empty lines, then trim. It is render-layer behavior
// reproduced here so both legs feed get_env the same template. BAML's leading-
// whitespace scan uses Rust `char::is_whitespace()` (Unicode White_Space), so the
// scan here uses unicode.IsSpace (the same set) to match BAML's dedent exactly,
// not merely strings.TrimSpace.
func dedentAndTrim(s string) string {
	lines := strings.Split(s, "\n")

	minIndent := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := 0
		for _, r := range line {
			if unicode.IsSpace(r) {
				indent++
			} else {
				break
			}
		}
		if minIndent < 0 || indent < minIndent {
			minIndent = indent
		}
	}
	if minIndent < 0 {
		minIndent = 0
	}

	for i, line := range lines {
		runes := []rune(line)
		if len(runes) >= minIndent {
			lines[i] = string(runes[minIndent:])
		} else {
			lines[i] = ""
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// RenderProfile renders a row through the profile (fork) leg and returns the
// rendered bytes, after reproducing BAML's template preprocessing. ctx.output_
// format is empty for every corpus function (all return string, which has no
// schema), so Config carries an empty OutputFormat — the same value BAML's ctx
// exposes for these functions.
func RenderProfile(r Row) (string, error) {
	src := dedentAndTrim(r.blockContent())
	tmpl, err := bamlprofile.New(bamlprofile.Config{}).TemplateFromNamedString("profile_oracle", src)
	if err != nil {
		return "", err
	}
	ctx := map[string]any{}
	for k, v := range r.Args {
		ctx[k] = v
	}
	return tmpl.Render(ctx)
}

// Message is a role + concatenated text, the canonical shape both legs reduce to
// for chat rows.
type Message struct {
	Role string
	Text string
}

// SplitChat reproduces the role split of nativeprompt/lower.go (BAML
// jinja-runtime/src/lib.rs:394-483) for the media-free corpus: split on the role
// delimiter, role markers open a message, other chunks are text (trimmed, empties
// dropped) appended to the current message. It is calibrated against the BAML leg
// (the authority), so it is oracle infrastructure, not a golden.
func SplitChat(rendered string) ([]Message, error) {
	var msgs []Message
	cur := -1
	for _, chunk := range strings.Split(rendered, roleDelimiter) {
		if strings.HasPrefix(chunk, roleMarkerStart) && strings.HasSuffix(chunk, roleMarkerEnd) {
			role, err := roleOfMarker(chunk)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, Message{Role: role})
			cur = len(msgs) - 1
			continue
		}
		text := strings.TrimSpace(chunk)
		if text == "" {
			continue
		}
		if cur < 0 {
			return nil, fmt.Errorf("profileoracle: content before first role marker: %q", text)
		}
		if msgs[cur].Text != "" {
			msgs[cur].Text += text
		} else {
			msgs[cur].Text = text
		}
	}
	return msgs, nil
}

// GenerateBAMLSource builds the deterministic in-memory .baml project for a
// corpus: one shared client plus one function per row. The map is filename ->
// content; it is hashed (source-map guard) and handed to CreateRuntime.
func GenerateBAMLSource(rows []Row) map[string]string {
	sorted := append([]Row(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	var fns strings.Builder
	for _, r := range sorted {
		fns.WriteString(functionSource(r))
		fns.WriteString("\n")
	}

	return map[string]string{
		"clients.baml":   clientSource(),
		"functions.baml": fns.String(),
	}
}

func clientSource() string {
	return "client<llm> " + clientName + " {\n" +
		"  provider openai\n" +
		"  options {\n" +
		"    model \"profile-oracle-model\"\n" +
		"    api_key \"profile-oracle-key\"\n" +
		"    base_url \"https://profile-oracle.invalid/v1\"\n" +
		"  }\n" +
		"}\n"
}

func functionSource(r Row) string {
	var b strings.Builder
	b.WriteString("function ")
	b.WriteString(r.FuncName())
	b.WriteString("(")
	for i, p := range r.Params {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p.Name)
		b.WriteString(": ")
		b.WriteString(p.BamlType)
	}
	b.WriteString(") -> string {\n")
	b.WriteString("  client ")
	b.WriteString(clientName)

	// The prompt body carries ARBITRARY caller text — rx() feeds any regex
	// pattern into it — so fence it with a collision-proof raw-string delimiter
	// (rawStringHashes) instead of a fixed #"..."#. A fixed single-hash fence would
	// let an interior "# silently terminate the raw string early, truncating the
	// generated .baml into an opaque CreateRuntime parse error.
	body := r.blockContent()
	n, ok := rawStringHashes(body)
	if !ok {
		panic(fmt.Sprintf("profileoracle: row %q prompt body cannot be embedded in a BAML raw string: "+
			"it contains a '\"' followed by 5+ '#', exceeding BAML's 5-hash maximum; body=%q", r.ID, body))
	}
	hashes := strings.Repeat("#", n)
	b.WriteString("\n  prompt ")
	b.WriteString(hashes)
	b.WriteString("\"")
	b.WriteString(body)
	b.WriteString("\"")
	b.WriteString(hashes)
	b.WriteString("\n}\n")
	return b.String()
}

// rawStringHashes returns how many '#' to fence a BAML raw block string with so
// that body embeds without premature termination, and whether BAML can hold it.
//
// BAML raw strings use Rust semantics (ast/src/parser/datamodel.pest,
// raw_string_literal): an opener of N '#' then '"', a closer of '"' then N '#',
// and content that may not contain a '"' followed by N-or-more '#' (the content
// rule is (!"\"<N '#'>" ~ ANY)*). So the fence needs N strictly greater than the
// longest run of '#' immediately following any '"' in the body; we pick that max
// run + 1. Runs of '#' NOT preceded by a '"' (and a trailing '"') are harmless.
// For body with no such "#… run the result is 1, reproducing the original
// #"..."# fence byte-for-byte.
//
// BAML supports at most 5 '#'. ok is false when the body needs more — it contains
// a '"' followed by 5+ '#', which NO delimiter can represent; the caller fails
// loud rather than emitting a truncated, mis-parsing .baml. ('"' and '#' are
// ASCII, never a UTF-8 continuation byte, so the byte scan is rune-safe.)
func rawStringHashes(body string) (n int, ok bool) {
	maxRun := 0
	for i := 0; i < len(body); i++ {
		if body[i] != '"' {
			continue
		}
		run := 0
		for j := i + 1; j < len(body) && body[j] == '#'; j++ {
			run++
		}
		if run > maxRun {
			maxRun = run
		}
	}
	n = maxRun + 1
	return n, n <= 5
}
