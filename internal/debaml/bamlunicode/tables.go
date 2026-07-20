package bamlunicode

import "sort"

// This file defines the in-memory shapes the generated tables (*_gen.go) are
// emitted against, plus the binary-search primitives the runtime uses to query
// them. The data itself is generated deterministically from pinned UCD inputs by
// ./cmd/gen; see doc.go and versions.go for the exact compatibility profile.
//
// Every table is emitted sorted and non-overlapping so lookups are a plain
// binary search with no allocation. None of these lookups consult the host Go
// runtime's Unicode tables, runtime.GOOS/GOARCH, or the network — the whole
// point of #555 is that the data is frozen to BAML's dual-version profile
// (case/properties = Unicode 17.0.0, normalization = Unicode 16.0.0).

// urange is an inclusive [lo, hi] scalar range used for boolean properties.
type urange struct{ lo, hi rune }

// lowerEntry is a single-scalar (1->1) lowercase mapping.
type lowerEntry struct{ from, to rune }

// lowerFullEntry is a one-to-many lowercase mapping sourced from an
// unconditional SpecialCasing.txt entry (e.g. U+0130 -> U+0069 U+0307).
type lowerFullEntry struct {
	from rune
	seq  []rune
}

// cccEntry assigns a canonical combining class to an inclusive scalar range.
type cccEntry struct {
	lo, hi rune
	class  uint8
}

// nfkdIdx maps a scalar to its fully-recursively-expanded compatibility
// decomposition, stored as a [off, off+length) window into nfkdPool.
type nfkdIdx struct {
	cp, off, length int32
}

// inRanges reports whether r falls in any range of a sorted, non-overlapping
// []urange via binary search.
func inRanges(ranges []urange, r rune) bool {
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i].hi >= r })
	return i < len(ranges) && ranges[i].lo <= r
}

// lookupLowerSingle returns the 1->1 lowercase mapping for r, or r unchanged.
func lookupLowerSingle(r rune) rune {
	i := sort.Search(len(lowerSingle), func(i int) bool { return lowerSingle[i].from >= r })
	if i < len(lowerSingle) && lowerSingle[i].from == r {
		return lowerSingle[i].to
	}
	return r
}

// lookupLowerFull returns the one-to-many lowercase mapping for r, if any.
func lookupLowerFull(r rune) ([]rune, bool) {
	i := sort.Search(len(lowerFull), func(i int) bool { return lowerFull[i].from >= r })
	if i < len(lowerFull) && lowerFull[i].from == r {
		return lowerFull[i].seq, true
	}
	return nil, false
}

// combiningClass returns the Unicode 16.0.0 canonical combining class of r
// (0 for starters and unassigned scalars).
func combiningClass(r rune) uint8 {
	i := sort.Search(len(ccc16), func(i int) bool { return ccc16[i].hi >= r })
	if i < len(ccc16) && ccc16[i].lo <= r {
		return ccc16[i].class
	}
	return 0
}

// lookupNFKD returns the fully-expanded compatibility decomposition of r, if any.
func lookupNFKD(r rune) ([]rune, bool) {
	cp := int32(r)
	i := sort.Search(len(nfkdIndex), func(i int) bool { return nfkdIndex[i].cp >= cp })
	if i < len(nfkdIndex) && nfkdIndex[i].cp == cp {
		e := nfkdIndex[i]
		return nfkdPool[e.off : e.off+e.length], true
	}
	return nil, false
}
