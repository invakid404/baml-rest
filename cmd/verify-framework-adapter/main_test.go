package main

import "testing"

// TestRewriteRelativeReplaceLine pins the contract for the single-line
// rewriter: only genuinely-relative go.mod replace targets ("./",
// "../", or exact ".") get rewritten; module-path replaces and
// absolute-path replaces pass through verbatim. CR verdict-2 flagged
// the prior `strings.Index(line, "=> ..")` form as too narrow — these
// cases lock in the broadened detection.
func TestRewriteRelativeReplaceLine(t *testing.T) {
	const origDir = "/repo/adapters/adapter_v0_215_0"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "parent-dir replace (the shape every shipping adapter uses)",
			in:   "\tgithub.com/invakid404/baml-rest/adapters/common => ../common",
			want: "\tgithub.com/invakid404/baml-rest/adapters/common => /repo/adapters/common",
		},
		{
			name: "deeper parent-dir replace",
			in:   "\tgithub.com/invakid404/baml-rest/bamlutils => ../../bamlutils",
			want: "\tgithub.com/invakid404/baml-rest/bamlutils => /repo/bamlutils",
		},
		{
			name: "same-dir relative replace (./ — previously skipped)",
			in:   "\texample.com/local => ./local",
			want: "\texample.com/local => /repo/adapters/adapter_v0_215_0/local",
		},
		{
			name: "exact dot relative replace (. — previously skipped)",
			in:   "\texample.com/self => .",
			want: "\texample.com/self => /repo/adapters/adapter_v0_215_0",
		},
		{
			// Codex sign-off NO-GO blocker on the v2 fix: the new
			// HasPrefix("../") check missed exact ".." (no trailing
			// slash). The previous substring matcher caught it by
			// accident; the predicate has to spell it out.
			name: "exact double-dot relative replace (.. — verdict-2 v3 gap)",
			in:   "\texample.com/parent => ..",
			want: "\texample.com/parent => /repo/adapters",
		},
		{
			name: "module-path replace passes through (no rewrite)",
			in:   "\tgithub.com/foo/bar => github.com/foo/bar-fork v1.2.3",
			want: "\tgithub.com/foo/bar => github.com/foo/bar-fork v1.2.3",
		},
		{
			name: "absolute-path replace passes through (no rewrite)",
			in:   "\texample.com/abs => /usr/local/src/abs",
			want: "\texample.com/abs => /usr/local/src/abs",
		},
		{
			name: "non-replace line passes through (no rewrite)",
			in:   "\tgithub.com/foo/bar v1.2.3",
			want: "\tgithub.com/foo/bar v1.2.3",
		},
		{
			name: "preserves trailing version on a relative replace",
			in:   "\texample.com/x => ../sibling v0.0.0",
			want: "\texample.com/x => /repo/adapters/sibling v0.0.0",
		},
		{
			name: "preserves trailing comment on a relative replace",
			in:   "\texample.com/x => ../sibling // see #199",
			want: "\texample.com/x => /repo/adapters/sibling // see #199",
		},
		{
			name: "single-line replace directive (outside a block)",
			in:   "replace example.com/x => ../sibling",
			want: "replace example.com/x => /repo/adapters/sibling",
		},
		{
			name: "tab-padded between => and path is preserved",
			in:   "\texample.com/x =>\t../sibling",
			want: "\texample.com/x =>\t/repo/adapters/sibling",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteRelativeReplaceLine(tc.in, origDir)
			if got != tc.want {
				t.Errorf("rewriteRelativeReplaceLine\n  in:   %q\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsRelativeReplacePath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{".", true},
		{"..", true}, // verdict-2 v3 gap: exact ".." was missed by HasPrefix("../")
		{"./local", true},
		{"./", true},
		{"../common", true},
		{"../../bamlutils", true},
		{"github.com/foo/bar", false},
		{"/usr/local/src", false},
		{"", false},
		{"foo/bar", false}, // bare path without ./ prefix isn't a go.mod relative replace
		{"..foo", false},   // doesn't have a "../" prefix
		{".foo", false},    // dot-prefixed file name, not a relative dir
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := isRelativeReplacePath(tc.in); got != tc.want {
				t.Errorf("isRelativeReplacePath(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
