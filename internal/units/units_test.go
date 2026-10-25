package units

import "testing"

func TestParseBytes(t *testing.T) {
	tests := []struct {
		input string
		want  int64
		ok    bool
	}{
		{"20GB", 20 << 30, true},
		{"20 GB", 20 << 30, true},
		{"1.5tb", 3 << 39, true},
		{"512MB", 512 << 20, true},
		{"10mib", 10 << 20, true},
		{"1024", 1024, true},
		{"0", 0, false},
		{"-1GB", 0, false},
		{"wat", 0, false},
		{"1.25", 0, false},
	}
	for _, test := range tests {
		got, err := ParseBytes(test.input)
		if test.ok && err != nil {
			t.Errorf("ParseBytes(%q) unexpected error: %v", test.input, err)
		}
		if !test.ok && err == nil {
			t.Errorf("ParseBytes(%q) expected error, got %d", test.input, got)
		}
		if test.ok && got != test.want {
			t.Errorf("ParseBytes(%q) = %d, want %d", test.input, got, test.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	tests := map[int64]string{
		0:          "0 B",
		999:        "999 B",
		1024:       "1.00 KB",
		20 << 30:   "20.0 GB",
		1500 << 20: "1.46 GB",
	}
	for input, want := range tests {
		if got := FormatBytes(input); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", input, got, want)
		}
	}
}
