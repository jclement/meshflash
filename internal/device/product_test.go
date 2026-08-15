package device

import "testing"

// A board must be identifiable in both states.
//
// Bootloaders append a marker to the application's product string — a Seeed
// T1000-E reports "T1000-E" running and "T1000-E-BOOT" in its bootloader — so
// matching only the exact string identified the board in one state and left it
// as one of eight candidates in the other.
func TestTrimBootSuffix(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		trimmed bool
	}{
		{"T1000-E-BOOT", "T1000-E", true},
		{"T1000-E_BOOT", "T1000-E", true},
		{"T1000-E BOOT", "T1000-E", true},
		{"RAK4631BOOT", "RAK4631", true},
		{"HT-n5262-BOOTLOADER", "HT-n5262", true},
		// Not a suffix, must be left alone.
		{"T1000-E", "T1000-E", false},
		{"HT-n5262", "HT-n5262", false},
		// Degenerate: the whole string is the marker.
		{"BOOT", "BOOT", false},
	}
	for _, c := range cases {
		got, trimmed := trimBootSuffix(c.in)
		if got != c.want || trimmed != c.trimmed {
			t.Errorf("trimBootSuffix(%q) = %q,%v want %q,%v", c.in, got, trimmed, c.want, c.trimmed)
		}
	}
}

func TestMatchesProduct(t *testing.T) {
	known := []string{"T1000-E"}

	for _, product := range []string{"T1000-E", "T1000-E-BOOT", "t1000-e-boot"} {
		if !matchesProduct(known, product) {
			t.Errorf("%q did not match %v", product, known)
		}
	}
	for _, product := range []string{"HT-n5262", "T1000", "", "BOOT"} {
		if matchesProduct(known, product) {
			t.Errorf("%q should not match %v", product, known)
		}
	}
}
