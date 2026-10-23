package cli

import (
	"strings"
	"testing"
)

// TestHiddenAncestorTripsWarn pins the reviewer cases where the leaf sits
// low but an ancestor is close to its own ceiling. Reading usage only at the
// tightest static level reported the leaf's 10% and exited 0.
func TestHiddenAncestorTripsWarn(t *testing.T) {
	tests := []struct {
		fixture  string
		headline string
	}{
		{"usage_ancestor_tie",
			"nearest ceiling: memory 95.0% (950 of 1000 bytes) (1 limit not measured)"},
		{"usage_ancestor_looser",
			"nearest ceiling: memory 97.5% (1950 of 2000 bytes) (1 limit not measured)"},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			res := run("show", "4242", "--root", cgroupRoot(t, tc.fixture))
			if res.code != 1 {
				t.Fatalf("exit %d, want 1\n%s", res.code, res.stdout)
			}
			lines := strings.Split(res.stdout, "\n")
			if lines[1] != tc.headline || lines[2] != "warning: 1 reading at or above --warn 90%" {
				t.Errorf("head = %q", lines[:3])
			}
		})
	}
}

func TestRatioVersusHeadroomShowsTheParentAndNamesTheLeaf(t *testing.T) {
	// The parent is at exactly 90%, which trips the default threshold; the
	// least-headroom note names the leaf but does not change what is shown.
	res := run("show", "4242", "--root", cgroupRoot(t, "usage_ratio_vs_headroom"))
	if res.code != 1 {
		t.Fatalf("exit %d, want 1", res.code)
	}
	if got := strings.Split(res.stdout, "\n")[1]; got != "nearest ceiling: memory 90.0% (90000 of 100000 bytes) (1 limit not measured)" {
		t.Errorf("headline = %q", got)
	}
	if !strings.Contains(res.stdout, "least headroom: /a/b (500 bytes left)") {
		t.Errorf("no least-headroom note:\n%s", res.stdout)
	}
}
