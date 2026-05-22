package testharness

import "testing"

func TestHasWindowsDrivePrefix(t *testing.T) {
	tt := []struct {
		name string
		want bool
	}{
		{"", false},
		{"C", false},
		{"C:", true},
		{"C:foo", true},
		{"c:foo", true},
		{"5:foo", false},
		{":foo", false},
		{"CC:foo", false},
		{"C:foo:bar", true},
		{"foo/C:bar", false},
	}
	for _, c := range tt {
		if got := hasWindowsDrivePrefix(c.name); got != c.want {
			t.Errorf("hasWindowsDrivePrefix(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
