package cgroup

import (
	"io/fs"
	"testing"

	"github.com/mabunemeh/limits-explain/internal/model"
)

func TestUsageIsReadAtTheLeafWhenTheLeafBinds(t *testing.T) {
	// cpu has no usage counter, so it stays unmeasured; memory.high shares
	// memory.current with memory.max.
	check(t, collect(t, fixture(t, "usage_basic")), []want{
		{key: "cgroup.memory.max", limit: u(1000), usage: u(620), status: model.StatusOK, unit: "bytes",
			contains: []string{"nearest its ceiling: /app (sys/fs/cgroup/app/memory.max)",
				"usage read at /app (sys/fs/cgroup/app/memory.current)"},
			absent: []string{"usage not measured", "subtree", "bound by", "tightest ceiling"}},
		{key: "cgroup.memory.high", limit: u(800), usage: u(620), status: model.StatusOK, unit: "bytes"},
		{key: "cgroup.pids.max", limit: u(100), usage: u(9), status: model.StatusOK, unit: "processes"},
		{key: "cgroup.cpu.max", status: model.StatusUnlimited, unit: "millicores"},
	})
}

func TestUsageCanExceedTheLimit(t *testing.T) {
	// The source reports the raw numbers; it does not clamp or flag.
	check(t, collect(t, fixture(t, "usage_over_limit")), []want{
		{key: "cgroup.memory.max", limit: u(1000), usage: u(1125), status: model.StatusOK, unit: "bytes"},
		{key: "cgroup.pids.max", limit: u(10), usage: u(10), status: model.StatusOK, unit: "processes"},
	})
}

func TestUsageIsReadAtTheNearestLevelNotTheLeaf(t *testing.T) {
	// The leaf's own counters (100 bytes, 3 tasks) are deliberately different
	// from the ancestor's (900, 40): reading the leaf would understate what
	// the ancestor's limit is enforced against.
	check(t, collect(t, fixture(t, "usage_ancestor_binds")), []want{
		{key: "cgroup.memory.max", limit: u(1000), usage: u(900), status: model.StatusOK, unit: "bytes",
			contains: []string{
				"nearest its ceiling: /a (sys/fs/cgroup/a/memory.max)",
				"usage read at /a (sys/fs/cgroup/a/memory.current), which counts its whole subtree",
				"leaf /a/b allows 5000",
			}, absent: []string{"tightest ceiling"}},
		{key: "cgroup.pids.max", limit: u(50), usage: u(40), status: model.StatusOK, unit: "processes",
			contains: []string{"sys/fs/cgroup/a/pids.current"}},
	})
}

func TestUsageNeverFallsBackToAnotherLevel(t *testing.T) {
	// The ancestor has no counter; the leaf does. The leaf's value must not
	// be borrowed, and the limit stays the batch-2 minimum (the leaf on a
	// tie) since the row is unmeasured.
	fsys := inMemory("0::/a/b\n", map[string]string{
		"sys/fs/cgroup/a/memory.max":       "1000\n",
		"sys/fs/cgroup/a/b/memory.current": "100\n",
	})
	check(t, collect(t, fsys), []want{
		{key: "cgroup.memory.max", limit: u(1000), status: model.StatusUnmeasured, unit: "bytes",
			contains: []string{"usage not measured: sys/fs/cgroup/a/memory.current absent"}},
	})
}

func TestUsageCounterProblemsLeaveTheReadingUnmeasured(t *testing.T) {
	tests := []struct {
		fixture  string
		memory   string // note fragment for memory.max
		pidsNote string // note fragment for pids.max, when its counter is bad too
		pidsUsed *uint64
	}{
		{"usage_current_missing", "usage not measured: sys/fs/cgroup/app/memory.current absent", "", u(9)},
		// "+9" is not a plain run of digits.
		{"usage_current_unparseable", `usage not measured: sys/fs/cgroup/app/memory.current unparseable: "lots"`,
			`pids.current unparseable: "+9"`, nil},
		{"usage_current_unreadable", "usage not measured: sys/fs/cgroup/app/memory.current unreadable: ", "", u(9)},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			pidsStatus := model.StatusOK
			var pidsContains []string
			if tc.pidsUsed == nil {
				pidsStatus = model.StatusUnmeasured
				pidsContains = []string{tc.pidsNote}
			}
			check(t, collect(t, fixture(t, tc.fixture)), []want{
				{key: "cgroup.memory.max", limit: u(1000), status: model.StatusUnmeasured, unit: "bytes",
					contains: []string{tc.memory}},
				{key: "cgroup.pids.max", limit: u(100), usage: tc.pidsUsed, status: pidsStatus, unit: "processes",
					contains: pidsContains},
			})
		})
	}
}

func TestUsageParsingIsStrict(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    *uint64
	}{
		{"plain", "5\n", u(5)},
		{"surrounding whitespace", " 5 \r\n", u(5)},
		{"zero", "0\n", u(0)},
		{"max uint64", "18446744073709551615\n", u(18446744073709551615)},
		{"overflow", "18446744073709551616\n", nil},
		{"signed", "-5\n", nil},
		{"hex", "0x5\n", nil},
		{"float", "5.0\n", nil},
		{"underscore", "1_000\n", nil},
		{"empty", "", nil},
		{"max word", "max\n", nil},
		{"non-ASCII digit", "٥\n", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := inMemory("0::/a\n", map[string]string{
				"sys/fs/cgroup/a/pids.max":     "100\n",
				"sys/fs/cgroup/a/pids.current": tc.content,
			})
			st := model.StatusUnmeasured
			if tc.want != nil {
				st = model.StatusOK
			}
			check(t, collect(t, fsys), []want{
				{key: "cgroup.pids.max", limit: u(100), usage: tc.want, status: st, unit: "processes"},
			})
		})
	}
}

func TestUsageUnreadableByPermission(t *testing.T) {
	fsys := errorFS{fixture(t, "usage_basic"), "/memory.current", fs.ErrPermission}
	check(t, collect(t, fsys), []want{
		{key: "cgroup.memory.max", limit: u(1000), status: model.StatusUnmeasured, unit: "bytes",
			contains: []string{"memory.current unreadable: permission denied"}},
		{key: "cgroup.memory.high", limit: u(800), status: model.StatusUnmeasured, unit: "bytes"},
		{key: "cgroup.pids.max", limit: u(100), usage: u(9), status: model.StatusOK, unit: "processes"},
		{key: "cgroup.cpu.max", status: model.StatusUnlimited, unit: "millicores"},
	})
}

func TestUsageNotReadForUnlimitedOrWithheldReadings(t *testing.T) {
	// "max" means no ceiling: a stray counter must not produce usage.
	fsys := inMemory("0::/a\n", map[string]string{
		"sys/fs/cgroup/a/memory.max":     "max\n",
		"sys/fs/cgroup/a/memory.current": "5\n",
		"sys/fs/cgroup/a/pids.max":       "abc\n",
		"sys/fs/cgroup/a/pids.current":   "5\n",
	})
	check(t, collect(t, fsys), []want{
		{key: "cgroup.memory.max", status: model.StatusUnlimited, unit: "bytes"},
		{key: "cgroup.pids.max", status: model.StatusUnparseable, unit: "processes"},
	})
}

func TestUsageValuesAbove2Pow53(t *testing.T) {
	check(t, collect(t, fixture(t, "usage_huge")), []want{
		{key: "cgroup.memory.max", limit: u(18446744073709551614), usage: u(18446744073709551613),
			status: model.StatusOK, unit: "bytes"},
		{key: "cgroup.pids.max", limit: u(9007199254740993), usage: u(9007199254740992),
			status: model.StatusOK, unit: "processes"},
	})
}

func TestAncestorHiddenBehindLeafAtTheSameLimit(t *testing.T) {
	// Leaf 100/1000 (10%) under /a 950/1000 (95%): the tie on the limit goes
	// to the leaf for the static ceiling, but the row must show /a.
	check(t, collect(t, fixture(t, "usage_ancestor_tie")), []want{
		{key: "cgroup.memory.max", limit: u(1000), usage: u(950), status: model.StatusOK, unit: "bytes",
			contains: []string{
				"nearest its ceiling: /a (sys/fs/cgroup/a/memory.max)",
				"usage read at /a (sys/fs/cgroup/a/memory.current), which counts its whole subtree",
				"tightest ceiling: /a/b allows 1000",
			},
			absent: []string{"least headroom"}},
	})
}

func TestAncestorHiddenBehindATighterLeaf(t *testing.T) {
	// Leaf 100/1000 (10%) under /a 1950/2000 (97.5%).
	check(t, collect(t, fixture(t, "usage_ancestor_looser")), []want{
		{key: "cgroup.memory.max", limit: u(2000), usage: u(1950), status: model.StatusOK, unit: "bytes",
			contains: []string{
				"nearest its ceiling: /a (sys/fs/cgroup/a/memory.max)",
				"tightest ceiling: /a/b allows 1000",
			},
			// /a also has the least absolute headroom (50 vs 900 left), so
			// there is nothing further to name.
			absent: []string{"least headroom"}},
	})
}

func TestRatioAndHeadroomDisagree(t *testing.T) {
	// Parent 90000/100000 (90%, 10000 left) vs leaf 500/1000 (50%, 500
	// left). The ratio decides what is shown; the leaf is only named.
	check(t, collect(t, fixture(t, "usage_ratio_vs_headroom")), []want{
		{key: "cgroup.memory.max", limit: u(100000), usage: u(90000), status: model.StatusOK, unit: "bytes",
			contains: []string{
				"nearest its ceiling: /a (sys/fs/cgroup/a/memory.max)",
				"tightest ceiling: /a/b allows 1000",
				"least headroom: /a/b (500 bytes left)",
			}},
	})
}

func TestRatioTieTakesTheRootmostLevel(t *testing.T) {
	got := collect(t, fixture(t, "usage_ratio_tie"))
	check(t, got, []want{
		{key: "cgroup.memory.max", limit: u(2000), usage: u(1000), status: model.StatusOK, unit: "bytes",
			contains: []string{"nearest its ceiling: /a (", "tightest ceiling: /a/b allows 1000",
				"least headroom: /a/b (500 bytes left)"}},
	})
}

func TestLeastHeadroomIsNotNamedWhenTheShownLevelHasIt(t *testing.T) {
	fsys := inMemory("0::/a/b\n", map[string]string{
		"sys/fs/cgroup/a/memory.max":       "1000\n",
		"sys/fs/cgroup/a/memory.current":   "100\n",
		"sys/fs/cgroup/a/b/memory.max":     "100\n",
		"sys/fs/cgroup/a/b/memory.current": "50\n",
		"sys/fs/cgroup/memory.max":         "max\n",
	})
	// Leaf 50% vs /a 10%: the leaf is shown and also has the least headroom.
	check(t, collect(t, fsys), []want{
		{key: "cgroup.memory.max", limit: u(100), usage: u(50), status: model.StatusOK, unit: "bytes",
			absent: []string{"least headroom", "tightest ceiling"}},
	})
}

func TestEqualHeadroomIsNotNamed(t *testing.T) {
	fsys := inMemory("0::/a/b\n", map[string]string{
		"sys/fs/cgroup/a/memory.max":       "500\n",
		"sys/fs/cgroup/a/memory.current":   "400\n",
		"sys/fs/cgroup/a/b/memory.max":     "1000\n",
		"sys/fs/cgroup/a/b/memory.current": "900\n",
		"sys/fs/cgroup/memory.max":         "max\n",
	})
	// Leaf 90% vs /a 80%, both with 100 left: only STRICTLY less headroom
	// earns a note, so the shown leaf stands alone.
	check(t, collect(t, fsys), []want{
		{key: "cgroup.memory.max", limit: u(1000), usage: u(900), status: model.StatusOK, unit: "bytes",
			contains: []string{"tightest ceiling: /a allows 500"},
			absent:   []string{"least headroom"}},
	})
}

func TestAnyFiniteLevelOverTheLimitIsShown(t *testing.T) {
	fsys := inMemory("0::/a/b\n", map[string]string{
		"sys/fs/cgroup/a/memory.max":       "10\n",
		"sys/fs/cgroup/a/memory.current":   "0\n",
		"sys/fs/cgroup/a/b/memory.max":     "0\n", // zero limit: fraction 1
		"sys/fs/cgroup/a/b/memory.current": "0\n",
	})
	check(t, collect(t, fsys), []want{
		{key: "cgroup.memory.max", limit: u(0), usage: u(0), status: model.StatusOK, unit: "bytes"},
	})
}

func TestFiniteCPUStaysUnmeasuredBesideMemoryCurrent(t *testing.T) {
	check(t, collect(t, fixture(t, "usage_cpu_finite")), []want{
		{key: "cgroup.memory.max", limit: u(1000), usage: u(620), status: model.StatusOK, unit: "bytes"},
		{key: "cgroup.cpu.max", limit: u(200), status: model.StatusUnmeasured, unit: "millicores",
			absent: []string{"usage"}},
	})
}
