package memlimit

import (
	"testing"
)

func TestParseBytes(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		// Raw bytes
		{"123", 123, false},
		{"0", 0, false},
		{"1B", 1, false},

		// Decimal (1000-based)
		{"1KB", 1000, false},
		{"1MB", 1000 * 1000, false},
		{"1GB", 1000 * 1000 * 1000, false},
		{"1TB", 1000 * 1000 * 1000 * 1000, false},
		{"500MB", 500 * 1000 * 1000, false},

		// Binary (1024-based) - IEC standard
		{"1KiB", 1024, false},
		{"1MiB", 1024 * 1024, false},
		{"1GiB", 1024 * 1024 * 1024, false},
		{"1TiB", 1024 * 1024 * 1024 * 1024, false},
		{"512MiB", 512 * 1024 * 1024, false},

		// Binary shorthand (Ki, Mi, Gi, Ti)
		{"1Ki", 1024, false},
		{"1Mi", 1024 * 1024, false},
		{"1Gi", 1024 * 1024 * 1024, false},
		{"1Ti", 1024 * 1024 * 1024 * 1024, false},

		// Single letter shorthand (K, M, G, T) - binary
		{"1K", 1024, false},
		{"1M", 1024 * 1024, false},
		{"1G", 1024 * 1024 * 1024, false},
		{"1T", 1024 * 1024 * 1024 * 1024, false},
		{"2G", 2 * 1024 * 1024 * 1024, false},

		// Case insensitivity
		{"1kib", 1024, false},
		{"1KIB", 1024, false},
		{"1mib", 1024 * 1024, false},
		{"1gb", 1000 * 1000 * 1000, false},

		// Whitespace handling
		{"  123  ", 123, false},
		{"1 GiB", 1024 * 1024 * 1024, false},

		// Special values
		{"off", 0, false},
		{"OFF", 0, false},
		{"Off", 0, false},

		// Error cases
		{"", 0, true},              // empty
		{"abc", 0, true},           // no number
		{"1.5GiB", 0, true},        // decimal not supported
		{"1.0MB", 0, true},         // decimal not supported
		{"-1GB", 0, true},          // negative (no number found)
		{"1XB", 0, true},           // unknown suffix
		{"9999999999999999999T", 0, true}, // overflow
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseBytes(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseBytes(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseBytes(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0B"},
		{512, "512B"},
		{1023, "1023B"},
		{1024, "1.00KiB"},
		{1536, "1.50KiB"},
		{1024 * 1024, "1.00MiB"},
		{1024 * 1024 * 1024, "1.00GiB"},
		{2 * 1024 * 1024 * 1024, "2.00GiB"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := FormatBytes(tt.input)
			if got != tt.want {
				t.Errorf("FormatBytes(%d) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
