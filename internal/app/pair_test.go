package app

import "testing"

func TestVersionLess(t *testing.T) {
	for _, c := range [][2]string{{"2.2.1", "2.3.2"}, {"3.9.0", "3.10.0"}, {"4.11.3_1", "4.14"}, {"1.35", "1.35_1"}} {
		if !versionLess(c[0], c[1]) || versionLess(c[1], c[0]) {
			t.Errorf("%s < %s", c[0], c[1])
		}
	}
	for _, n := range []string{"2.3.2", "v1.0", "3.7c", "2026.7.22", "1.13.2_1"} {
		if !versionDir.MatchString(n) {
			t.Errorf("%q should look like a version", n)
		}
	}
	for _, n := range []string{"bin", "share", "tree"} {
		if versionDir.MatchString(n) {
			t.Errorf("%q should not look like a version", n)
		}
	}
}
