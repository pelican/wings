package plugin

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.0", "1.2.0", 0},
		{"1.10.0", "1.9.0", 1}, // numeric, not lexical
		{"1.9.0", "1.10.0", -1},
		{"2.0.0", "1.9.9", 1},
		{"v1.2.0", "1.2.0", 0}, // v prefix ignored
		{"1.2.1", "1.2", 1},
		{"1.2", "1.2.0", 0}, // missing segments are zero
		{"1.3.0", "1.2.9", 1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestVersionNewer(t *testing.T) {
	if !versionNewer("1.3.0", "1.2.9") {
		t.Error("1.3.0 should be newer than 1.2.9")
	}
	if versionNewer("1.2.0", "1.2.0") {
		t.Error("equal versions are not newer")
	}
	if versionNewer("1.1.0", "1.2.0") {
		t.Error("older version must not be newer")
	}
}
