package bamlunicode

// Algorithmic Hangul decomposition constants (Unicode 3.12, stable across
// versions). Hangul syllables are decomposed by formula rather than table.
const (
	hangulSBase  = 0xAC00
	hangulLBase  = 0x1100
	hangulVBase  = 0x1161
	hangulTBase  = 0x11A7
	hangulLCount = 19
	hangulVCount = 21
	hangulTCount = 28
	hangulNCount = hangulVCount * hangulTCount // 588
	hangulSCount = hangulLCount * hangulNCount // 11172
)

// NFKD returns the Unicode Normalization Form KD of s, byte-for-byte identical
// to unicode-normalization 0.1.24 (Unicode 16.0.0).
//
// The algorithm is the standard one:
//
//  1. Full compatibility decomposition of each scalar. The committed table
//     (nfkdIndex/nfkdPool) already holds the RECURSIVE expansion, so a single
//     lookup yields the fully-decomposed sequence; Hangul syllables are
//     decomposed by formula instead.
//  2. Canonical ordering: within each maximal run of non-starters (canonical
//     combining class > 0) the scalars are stably sorted by combining class.
//
// Combining-mark removal (BAML applies IsCombiningMark after NFKD) is a separate
// primitive; NFKD itself does not strip marks.
func NFKD(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		out = appendDecomposed(out, r)
	}
	canonicalOrder(out)
	return string(out)
}

// appendDecomposed appends the full compatibility decomposition of r to dst.
func appendDecomposed(dst []rune, r rune) []rune {
	if r >= hangulSBase && r < hangulSBase+hangulSCount {
		return appendHangulDecomposition(dst, r)
	}
	if seq, ok := lookupNFKD(r); ok {
		return append(dst, seq...)
	}
	return append(dst, r)
}

// appendHangulDecomposition appends the algorithmic L(+V)(+T) decomposition of a
// Hangul syllable to dst.
func appendHangulDecomposition(dst []rune, s rune) []rune {
	sIndex := s - hangulSBase
	l := hangulLBase + sIndex/hangulNCount
	v := hangulVBase + (sIndex%hangulNCount)/hangulTCount
	t := hangulTBase + sIndex%hangulTCount
	dst = append(dst, l, v)
	if t != hangulTBase {
		dst = append(dst, t)
	}
	return dst
}

// canonicalOrder applies the Unicode canonical ordering algorithm in place: each
// maximal run of non-starters (combining class > 0) is stably sorted by
// combining class. Starters (class 0) delimit runs and never move.
func canonicalOrder(runes []rune) {
	n := len(runes)
	for i := 0; i < n; {
		if combiningClass(runes[i]) == 0 {
			i++
			continue
		}
		j := i + 1
		for j < n && combiningClass(runes[j]) != 0 {
			j++
		}
		// Stable insertion sort of runes[i:j] by combining class.
		for k := i + 1; k < j; k++ {
			r := runes[k]
			cc := combiningClass(r)
			m := k
			for m > i && combiningClass(runes[m-1]) > cc {
				runes[m] = runes[m-1]
				m--
			}
			runes[m] = r
		}
		i = j
	}
}
