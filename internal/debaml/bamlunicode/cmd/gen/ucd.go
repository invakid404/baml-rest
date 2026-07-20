package main

import (
	"bufio"
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// urange is an inclusive [lo, hi] scalar range (mirrors the runtime type).
type urange struct{ lo, hi rune }

// ucdRecord is one explicit UnicodeData.txt row.
type ucdRecord struct {
	cp       rune
	gc       string
	ccc      uint8
	simpleLC rune
	hasLC    bool
	decomp   []rune // one-step decomposition (canonical or compat; tag stripped)
}

// unicodeData is a parsed UnicodeData.txt: explicit rows plus the general
// category assigned to First..Last range rows (CJK, Hangul, PUA, surrogates).
type unicodeData struct {
	records map[rune]*ucdRecord
	gcRange []struct {
		lo, hi rune
		gc     string
	}
}

// gcOf returns the general category of cp ("Cn" if unassigned).
func (u *unicodeData) gcOf(cp rune) string {
	if r, ok := u.records[cp]; ok {
		return r.gc
	}
	i := sort.Search(len(u.gcRange), func(i int) bool { return u.gcRange[i].hi >= cp })
	if i < len(u.gcRange) && u.gcRange[i].lo <= cp {
		return u.gcRange[i].gc
	}
	return "Cn"
}

func parseHex(s string) (rune, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 16, 32)
	if err != nil {
		return 0, err
	}
	return rune(v), nil
}

// parseHexSeq parses a space-separated list of hex scalars ("0069 0307").
func parseHexSeq(s string) ([]rune, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []rune
	for _, tok := range strings.Fields(s) {
		r, err := parseHex(tok)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// parseUnicodeData parses UnicodeData.txt bytes, expanding First/Last range rows.
// path is a display label for error messages.
func parseUnicodeData(path string, data []byte) (*unicodeData, error) {
	u := &unicodeData{records: make(map[rune]*ucdRecord)}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)

	var rangeStart rune
	var rangeStartName, rangeStartGC string
	inRange := false

	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ";")
		if len(fields) < 15 {
			return nil, fmt.Errorf("%s: short line: %q", path, line)
		}
		cp, err := parseHex(fields[0])
		if err != nil {
			return nil, fmt.Errorf("%s: bad code point %q: %w", path, fields[0], err)
		}
		name := fields[1]
		gc := fields[2]

		// Range rows come in <..., First>/<..., Last> pairs sharing a general
		// category and carrying no case/decomposition data.
		if strings.HasSuffix(name, ", First>") {
			rangeStart = cp
			rangeStartName = name
			rangeStartGC = gc
			inRange = true
			continue
		}
		if strings.HasSuffix(name, ", Last>") {
			if !inRange {
				return nil, fmt.Errorf("%s: Last without First at %04X", path, cp)
			}
			if gc != rangeStartGC {
				return nil, fmt.Errorf("%s: range GC mismatch %s..%s", path, rangeStartName, name)
			}
			u.gcRange = append(u.gcRange, struct {
				lo, hi rune
				gc     string
			}{rangeStart, cp, gc})
			inRange = false
			continue
		}

		rec := &ucdRecord{cp: cp, gc: gc}
		if v, err := strconv.ParseUint(fields[3], 10, 8); err == nil {
			rec.ccc = uint8(v)
		} else {
			return nil, fmt.Errorf("%s: bad ccc %q: %w", path, fields[3], err)
		}
		// Field 5: decomposition mapping. A leading <tag> marks a compatibility
		// decomposition; NFKD applies both canonical and compatibility mappings,
		// so we keep the sequence either way (tag stripped).
		if d := strings.TrimSpace(fields[5]); d != "" {
			if strings.HasPrefix(d, "<") {
				if idx := strings.IndexByte(d, '>'); idx >= 0 {
					d = strings.TrimSpace(d[idx+1:])
				}
			}
			rec.decomp, err = parseHexSeq(d)
			if err != nil {
				return nil, fmt.Errorf("%s: bad decomposition %q: %w", path, fields[5], err)
			}
		}
		// Field 13: simple lowercase mapping.
		if lc := strings.TrimSpace(fields[13]); lc != "" {
			r, err := parseHex(lc)
			if err != nil {
				return nil, fmt.Errorf("%s: bad lowercase %q: %w", path, lc, err)
			}
			rec.simpleLC = r
			rec.hasLC = true
		}
		u.records[cp] = rec
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Slice(u.gcRange, func(i, j int) bool { return u.gcRange[i].lo < u.gcRange[j].lo })
	return u, nil
}

// specialLower holds unconditional full lowercase mappings from SpecialCasing.txt.
type specialLower struct {
	full map[rune][]rune
}

// knownLanguages are the locale tags whose SpecialCasing tailorings are
// intentionally excluded (BAML's matcher is language-independent).
var knownLanguages = map[string]bool{"lt": true, "tr": true, "az": true}

// parseSpecialCasing parses SpecialCasing.txt and returns the UNCONDITIONAL full
// lowercase mappings. It FAILS if it encounters a language-independent
// conditional lowercase mapping other than the single hard-coded case
// (U+03A3 Final_Sigma), so a future UCD that adds such a condition cannot be
// silently mis-lowercased.
func parseSpecialCasing(path string, data []byte) (*specialLower, error) {
	out := &specialLower{full: make(map[rune][]rune)}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ";")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		if len(fields) < 4 {
			return nil, fmt.Errorf("%s: short line: %q", path, line)
		}
		cp, err := parseHex(fields[0])
		if err != nil {
			return nil, fmt.Errorf("%s: bad code point %q: %w", path, fields[0], err)
		}
		lower, err := parseHexSeq(fields[1])
		if err != nil {
			return nil, fmt.Errorf("%s: bad lower %q: %w", path, fields[1], err)
		}
		condition := ""
		if len(fields) >= 5 {
			condition = strings.TrimSpace(fields[4])
		}

		if condition == "" {
			out.full[cp] = lower
			continue
		}

		// Conditional mapping. Classify tokens: a language tag is lowercase
		// (e.g. "lt"); a condition name is TitleCase (e.g. "Final_Sigma").
		hasLanguage := false
		for _, tok := range strings.Fields(condition) {
			if tok == "" {
				continue
			}
			if tok[0] >= 'a' && tok[0] <= 'z' {
				hasLanguage = true
				if !knownLanguages[tok] {
					return nil, fmt.Errorf("%s: unknown locale tag %q (cp %04X): review before excluding", path, tok, cp)
				}
			}
		}
		if hasLanguage {
			// Locale-specific (Turkic/Lithuanian): intentionally excluded.
			continue
		}
		// Language-independent conditional lowercase. The only one BAML/Rust
		// implements is U+03A3 Final_Sigma, handled contextually in the runtime.
		if cp == 0x03A3 && condition == "Final_Sigma" {
			continue
		}
		return nil, fmt.Errorf("%s: unhandled language-independent lowercase condition %q for U+%04X; "+
			"the generator refuses to silently drop it", path, condition, cp)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// parseProperty parses a UCD property file (DerivedCoreProperties.txt,
// PropList.txt) and returns the coalesced scalar ranges assigned the named
// property.
func parseProperty(path, prop string, data []byte) ([]urange, error) {
	var ranges []urange
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ";")
		if len(fields) < 2 {
			continue
		}
		if strings.TrimSpace(fields[1]) != prop {
			continue
		}
		lo, hi, err := parseRange(strings.TrimSpace(fields[0]))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		ranges = append(ranges, urange{lo, hi})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return coalesce(ranges), nil
}

// parseRange parses "XXXX" or "XXXX..YYYY".
func parseRange(s string) (rune, rune, error) {
	if i := strings.Index(s, ".."); i >= 0 {
		lo, err := parseHex(s[:i])
		if err != nil {
			return 0, 0, err
		}
		hi, err := parseHex(s[i+2:])
		if err != nil {
			return 0, 0, err
		}
		return lo, hi, nil
	}
	v, err := parseHex(s)
	if err != nil {
		return 0, 0, err
	}
	return v, v, nil
}

// coalesce sorts and merges overlapping/adjacent ranges into a minimal set.
func coalesce(in []urange) []urange {
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool { return in[i].lo < in[j].lo })
	out := []urange{in[0]}
	for _, r := range in[1:] {
		last := &out[len(out)-1]
		if r.lo <= last.hi+1 {
			if r.hi > last.hi {
				last.hi = r.hi
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// gcRanges builds the coalesced ranges of code points whose general category is
// in the given set (e.g. {Nd,Nl,No} or {Mn,Mc,Me}).
func gcRanges(u *unicodeData, categories map[string]bool) []urange {
	var ranges []urange
	var cur urange
	have := false
	for cp := rune(0); cp <= 0x10FFFF; cp++ {
		if categories[u.gcOf(cp)] {
			if have && cp == cur.hi+1 {
				cur.hi = cp
			} else {
				if have {
					ranges = append(ranges, cur)
				}
				cur = urange{cp, cp}
				have = true
			}
		}
	}
	if have {
		ranges = append(ranges, cur)
	}
	return ranges
}
