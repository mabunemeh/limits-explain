package systemd

import (
	"io/fs"
	"math/big"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mabunemeh/limits-explain/internal/model"
	"github.com/mabunemeh/limits-explain/internal/sources/cgroup"
)

// assertOnlyNotesChanged fails if Annotate did anything but append to Notes:
// same rows, same order, same Status, Limit and Usage.
func assertOnlyNotesChanged(t *testing.T, before, after []model.LimitReading) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%d rows became %d: Annotate must not add or drop rows", len(before), len(after))
	}
	for i := range before {
		b, a := before[i], after[i]
		if a.Key != b.Key || a.Kind != b.Kind || a.Label != b.Label || a.Source != b.Source ||
			a.Limit != b.Limit || a.Usage != b.Usage || a.Unit != b.Unit || a.Status != b.Status {
			t.Errorf("row %s changed beyond its notes:\n  before %+v\n  after  %+v", b.Key, b, a)
		}
		if !strings.HasPrefix(a.Notes, b.Notes) {
			t.Errorf("row %s lost its notes:\n  before %q\n  after  %q", b.Key, b.Notes, a.Notes)
		}
	}
}

// added returns what Annotate appended to the notes of the row with key, or ""
// when the row is absent or untouched.
func added(before, after []model.LimitReading, key string) string {
	for i := range before {
		if before[i].Key != key {
			continue
		}
		rest := strings.TrimPrefix(after[i].Notes, before[i].Notes)
		if before[i].Notes != "" {
			rest = strings.TrimPrefix(rest, "; ")
		}
		return rest
	}
	return ""
}

func TestAnnotateRealUnits(t *testing.T) {
	tests := []struct {
		fixture string
		// want maps a row key to the exact text appended to its notes.
		want map[string]string
		// none lists rows that must carry no systemd text at all.
		none []string
	}{
		{
			fixture: "all_match",
			want: map[string]string{
				"cgroup.pids.max":     "systemd: TasksMax=7 on le-match.service",
				"cgroup.memory.max":   "systemd: MemoryMax=67108864 on le-match.service",
				"cgroup.memory.high":  "systemd: MemoryHigh=33554432 on le-match.service",
				"cgroup.cpu.max":      "systemd: CPUQuotaPerSecUSec=500ms on le-match.service",
				"rlimit.nofile.soft":  "systemd: LimitNOFILESoft=4096 on le-match.service",
				"rlimit.nofile.hard":  "systemd: LimitNOFILE=8192 on le-match.service",
				"rlimit.nproc.single": "systemd: LimitNPROC=1000 (soft and hard) on le-match.service",
				"rlimit.core.hard":    "systemd: LimitCORE=infinity on le-match.service",
				"rlimit.core.soft":    "systemd: LimitCORESoft=0 on le-match.service",
				"rlimit.stack.soft":   "systemd: LimitSTACKSoft=8388608 on le-match.service",
			},
		},
		{
			// The kernel clamps "infinity" to its own cap, so the directive
			// and the process disagree and the note says so.
			fixture: "nofile_infinity",
			want: map[string]string{
				"rlimit.nofile.single": "systemd: configured LimitNOFILE=infinity on le-nofinf.service, effective 2147483584" +
					"; systemd: configured LimitNOFILESoft=infinity on le-nofinf.service, effective 2147483584",
				"cgroup.cpu.max":  "systemd: CPUQuotaPerSecUSec=infinity on le-nofinf.service",
				"cgroup.pids.max": "systemd: TasksMax=18706 on le-nofinf.service",
			},
		},
		{
			// 7.5% at a 50ms period is 75ms per second; cpu.max is "3750 50000".
			fixture: "cpu_fraction",
			want: map[string]string{
				"cgroup.cpu.max": "systemd: CPUQuotaPerSecUSec=75ms (period 50ms) on le-cpufrac.service",
			},
		},
		{
			fixture: "cpu_150",
			want: map[string]string{
				"cgroup.cpu.max": "systemd: CPUQuotaPerSecUSec=1.500000s on le-cpu150.service",
			},
		},
		{
			// prlimit lowered the soft limit after the unit started.
			fixture: "rlimit_changed",
			want: map[string]string{
				"rlimit.nofile.soft": "systemd: configured LimitNOFILESoft=4096 on le-rlim.service, effective 512",
				"rlimit.nofile.hard": "systemd: LimitNOFILE=8192 on le-rlim.service",
			},
		},
		{
			// Something wrote pids.max directly after systemd set it.
			fixture: "pids_overwritten",
			want: map[string]string{
				"cgroup.pids.max": "systemd: configured TasksMax=7 on le-pids.service, effective 5",
			},
		},
		{
			// 10000001 became 9998336 = 2441 pages of 4096 when written.
			fixture: "memory_rounded",
			want: map[string]string{
				"cgroup.memory.max": "systemd: MemoryMax=10000001 on le-mem.service, " +
					"effective 9998336, consistent with rounding down to whole pages",
				"cgroup.memory.high": "systemd: MemoryHigh=10000001 on le-mem.service, " +
					"effective 9998336, consistent with rounding down to whole pages",
			},
		},
		{
			// 33.33% at a 7ms period is 2333.1 microseconds per period; systemd
			// floors it to 2333, so the exact cross-multiply fails and the
			// floored value is what matches.
			fixture: "cpu_floored",
			want: map[string]string{
				"cgroup.cpu.max": "systemd: CPUQuotaPerSecUSec=333.300ms (period 7ms) on le-floor.service, " +
					"effective 2333us per 7000us, consistent with the quota being floored to a whole microsecond per period",
			},
		},
		{
			// A scope reports no MainPID and no Limit lines: only the cgroup
			// directives are annotated.
			fixture: "scope",
			want: map[string]string{
				"cgroup.pids.max": "systemd: TasksMax=9 on le-scope.scope",
			},
			none: []string{"rlimit.nofile.soft", "rlimit.nofile.hard", "rlimit.nofile.single"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			fsys := os.DirFS(fixtureDir(t, tc.fixture))
			before := collectAll(t, fsys)
			u, err := Query(replayFile(t, tc.fixture), "x.service")
			if err != nil {
				t.Fatal(err)
			}
			after := Annotate(before, u, fsys)
			assertOnlyNotesChanged(t, before, after)
			for key, want := range tc.want {
				if got := added(before, after, key); got != want {
					t.Errorf("%s\n  got  %q\n  want %q", key, got, want)
				}
			}
			for _, key := range tc.none {
				if got := added(before, after, key); got != "" {
					t.Errorf("%s was annotated: %q", key, got)
				}
			}
		})
	}
}

// unitFiles builds a tree holding the cgroup files of /system.slice/a.service.
func unitFiles(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for name, data := range files {
		m["sys/fs/cgroup/system.slice/a.service/"+name] = &fstest.MapFile{Data: []byte(data)}
	}
	return m
}

// unitOf builds a Unit from the properties of a hand-written show output.
func unitOf(t *testing.T, props string) *Unit {
	t.Helper()
	m, err := parseShow("Id=a.service\nLoadState=loaded\nControlGroup=/system.slice/a.service\n" + props)
	if err != nil {
		t.Fatal(err)
	}
	u, err := newUnit(m)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func row(key string, limit model.Amount, status model.Status, notes string) model.LimitReading {
	return model.LimitReading{Key: key, Kind: model.KindSingle, Source: "x", Limit: limit,
		Usage: model.UnknownAmount(), Status: status, Notes: notes}
}

func TestAnnotateScalarComparison(t *testing.T) {
	tests := []struct {
		name  string
		props string
		file  string // content of the unit-level file; "-" means the file is absent
		key   string
		want  string
	}{
		{"finite equal", "TasksMax=7\n", "7\n", "cgroup.pids.max", "systemd: TasksMax=7 on a.service"},
		{"finite differs", "TasksMax=7\n", "5\n", "cgroup.pids.max",
			"systemd: configured TasksMax=7 on a.service, effective 5"},
		{"finite vs max", "TasksMax=7\n", "max\n", "cgroup.pids.max",
			"systemd: configured TasksMax=7 on a.service, effective max (not applied)"},
		{"infinity vs max", "TasksMax=infinity\n", "max\n", "cgroup.pids.max",
			"systemd: TasksMax=infinity on a.service"},
		{"infinity vs finite", "TasksMax=infinity\n", "5\n", "cgroup.pids.max",
			"systemd: configured TasksMax=infinity on a.service, effective 5"},
		{"memory.high equal", "MemoryHigh=4096\n", "4096\n", "cgroup.memory.high",
			"systemd: MemoryHigh=4096 on a.service"},
		{"rounded down from an odd number", "MemoryMax=1000001\n", "999424\n", "cgroup.memory.max",
			"systemd: MemoryMax=1000001 on a.service, effective 999424, consistent with rounding down to whole pages"},
		{"not page aligned is a difference", "MemoryMax=1000001\n", "999423\n", "cgroup.memory.max",
			"systemd: configured MemoryMax=1000001 on a.service, effective 999423"},
		{"more than a page below is a difference", "MemoryMax=1000001\n", "995328\n", "cgroup.memory.max",
			"systemd: configured MemoryMax=1000001 on a.service, effective 995328"},
		{"above the directive is a difference", "MemoryMax=1000001\n", "1003520\n", "cgroup.memory.max",
			"systemd: configured MemoryMax=1000001 on a.service, effective 1003520"},
		{"64k pages", "MemoryMax=100000\n", "65536\n", "cgroup.memory.max",
			"systemd: MemoryMax=100000 on a.service, effective 65536, consistent with rounding down to whole pages"},
		{"tasks are never rounded", "TasksMax=1000001\n", "999424\n", "cgroup.pids.max",
			"systemd: configured TasksMax=1000001 on a.service, effective 999424"},
		{"file absent", "TasksMax=7\n", "-", "cgroup.pids.max",
			"systemd: TasksMax=7 on a.service, not compared: pids.max at the unit level: "},
		{"file unparseable", "TasksMax=7\n", "lots\n", "cgroup.pids.max",
			"systemd: TasksMax=7 on a.service, not compared: pids.max at the unit level: "},
		{"directive not understood", "TasksMax=lots\n", "7\n", "cgroup.pids.max",
			`systemd: TasksMax="lots" on a.service was not understood`},
		{"directive negative", "TasksMax=-1\n", "7\n", "cgroup.pids.max",
			`systemd: TasksMax="-1" on a.service was not understood`},
		{"directive overflows", "TasksMax=18446744073709551616\n", "7\n", "cgroup.pids.max",
			`systemd: TasksMax="18446744073709551616" on a.service was not understood`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			if tc.file != "-" {
				name := strings.TrimPrefix(tc.key, "cgroup.")
				files[name] = tc.file
			}
			before := []model.LimitReading{row(tc.key, model.KnownAmount(3), model.StatusUnmeasured, "")}
			after := Annotate(before, unitOf(t, tc.props), unitFiles(files))
			assertOnlyNotesChanged(t, before, after)
			got := after[0].Notes
			if strings.HasSuffix(tc.want, ": ") { // the tail is an OS error text
				if !strings.HasPrefix(got, tc.want) || len(got) == len(tc.want) {
					t.Errorf("notes = %q, want prefix %q and a reason", got, tc.want)
				}
				return
			}
			if got != tc.want {
				t.Errorf("notes\n  got  %q\n  want %q", got, tc.want)
			}
		})
	}
}

// TestAnnotateComparesAtTheUnitLevel pins the point of the unit-level read: a
// row shows the tightest level, which can be an ancestor, and the directive
// must be judged against the unit's own file, not against that row.
func TestAnnotateComparesAtTheUnitLevel(t *testing.T) {
	// The row displays an ancestor's limit of 3, tighter than the unit's own
	// pids.max of 7, which is exactly what TasksMax asked for.
	before := []model.LimitReading{row("cgroup.pids.max", model.KnownAmount(3), model.StatusUnmeasured,
		"bound by /system.slice (sys/fs/cgroup/system.slice/pids.max)")}
	after := Annotate(before, unitOf(t, "TasksMax=7\n"), unitFiles(map[string]string{"pids.max": "7\n"}))
	want := "bound by /system.slice (sys/fs/cgroup/system.slice/pids.max); systemd: TasksMax=7 on a.service"
	if after[0].Notes != want {
		t.Errorf("notes = %q, want %q", after[0].Notes, want)
	}
	if after[0].Limit != model.KnownAmount(3) {
		t.Errorf("Limit changed to %+v", after[0].Limit)
	}
}

func TestAnnotateCPU(t *testing.T) {
	tests := []struct {
		name  string
		props string
		file  string
		want  string
	}{
		{"equal", "CPUQuotaPerSecUSec=75ms\nCPUQuotaPeriodUSec=50ms\n", "3750 50000\n",
			"systemd: CPUQuotaPerSecUSec=75ms (period 50ms) on a.service"},
		{"equal at the default period", "CPUQuotaPerSecUSec=1.500000s\nCPUQuotaPeriodUSec=infinity\n", "150000 100000\n",
			"systemd: CPUQuotaPerSecUSec=1.500000s on a.service"},
		{"scaled period still equal", "CPUQuotaPerSecUSec=500ms\n", "5000 10000\n",
			"systemd: CPUQuotaPerSecUSec=500ms on a.service"},
		{"differs", "CPUQuotaPerSecUSec=75ms\n", "150000 100000\n",
			"systemd: configured CPUQuotaPerSecUSec=75ms on a.service, effective 150000us per 100000us"},
		{"a quota one above the floor", "CPUQuotaPerSecUSec=75020us\n", "3750 50000\n",
			"systemd: configured CPUQuotaPerSecUSec=75020us on a.service, effective 3750us per 50000us"},
		{"finite vs max", "CPUQuotaPerSecUSec=75ms\n", "max 100000\n",
			"systemd: configured CPUQuotaPerSecUSec=75ms on a.service, effective max (not applied)"},
		{"infinity vs max", "CPUQuotaPerSecUSec=infinity\n", "max 100000\n",
			"systemd: CPUQuotaPerSecUSec=infinity on a.service"},
		{"infinity vs finite", "CPUQuotaPerSecUSec=infinity\n", "20000 100000\n",
			"systemd: configured CPUQuotaPerSecUSec=infinity on a.service, effective 20000us per 100000us"},
		{"bad period text is left out", "CPUQuotaPerSecUSec=75ms\nCPUQuotaPeriodUSec=junk\n", "7500 100000\n",
			"systemd: CPUQuotaPerSecUSec=75ms on a.service"},
		{"not understood", "CPUQuotaPerSecUSec=1.5us\n", "3750 50000\n",
			`systemd: CPUQuotaPerSecUSec="1.5us" on a.service was not understood`},
		{"floored: 33.33% at 7ms", "CPUQuotaPerSecUSec=333.300ms\nCPUQuotaPeriodUSec=7ms\n", "2333 7000\n",
			"systemd: CPUQuotaPerSecUSec=333.300ms (period 7ms) on a.service, effective 2333us per 7000us, " +
				"consistent with the quota being floored to a whole microsecond per period"},
		{"floored: 7.33% at 15ms", "CPUQuotaPerSecUSec=73.300ms\nCPUQuotaPeriodUSec=15ms\n", "1099 15000\n",
			"systemd: CPUQuotaPerSecUSec=73.300ms (period 15ms) on a.service, effective 1099us per 15000us, " +
				"consistent with the quota being floored to a whole microsecond per period"},
		{"one above the floored value differs", "CPUQuotaPerSecUSec=333.300ms\n", "2334 7000\n",
			"systemd: configured CPUQuotaPerSecUSec=333.300ms on a.service, effective 2334us per 7000us"},
		{"one below the floored value differs", "CPUQuotaPerSecUSec=333.300ms\n", "2332 7000\n",
			"systemd: configured CPUQuotaPerSecUSec=333.300ms on a.service, effective 2332us per 7000us"},
		{"scaled period (0.5% at 100ms is 1000 per 200000)", "CPUQuotaPerSecUSec=5ms\nCPUQuotaPeriodUSec=100ms\n",
			"1000 200000\n", "systemd: CPUQuotaPerSecUSec=5ms (period 100ms) on a.service"},
		{"exact above 2^64 in both products", "CPUQuotaPerSecUSec=4000000000000us\n", "40000000000000 10000000\n",
			"systemd: CPUQuotaPerSecUSec=4000000000000us on a.service"},
		{"low words equal, high words differ", "CPUQuotaPerSecUSec=1s\n",
			"288230376152711744 1000000\n",
			"systemd: configured CPUQuotaPerSecUSec=1s on a.service, effective 288230376152711744us per 1000000us"},
		{"huge directive does not overflow", "CPUQuotaPerSecUSec=18446744073709551615us\n",
			"18446744073709551615 18446744073709551615\n",
			"systemd: configured CPUQuotaPerSecUSec=18446744073709551615us on a.service, " +
				"effective 18446744073709551615us per 18446744073709551615us"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := []model.LimitReading{row("cgroup.cpu.max", model.KnownAmount(1), model.StatusUnmeasured, "")}
			after := Annotate(before, unitOf(t, tc.props), unitFiles(map[string]string{"cpu.max": tc.file}))
			assertOnlyNotesChanged(t, before, after)
			if after[0].Notes != tc.want {
				t.Errorf("notes\n  got  %q\n  want %q", after[0].Notes, tc.want)
			}
		})
	}
	t.Run("file absent", func(t *testing.T) {
		before := []model.LimitReading{row("cgroup.cpu.max", model.KnownAmount(1), model.StatusUnmeasured, "")}
		after := Annotate(before, unitOf(t, "CPUQuotaPerSecUSec=75ms\n"), unitFiles(nil))
		if !strings.Contains(after[0].Notes, "not compared: cpu.max at the unit level") {
			t.Errorf("notes = %q", after[0].Notes)
		}
	})
}

func TestAnnotateRlimits(t *testing.T) {
	unlimited := func(key string) model.LimitReading { return row(key, model.UnknownAmount(), model.StatusUnlimited, "") }
	finite := func(key string, v uint64) model.LimitReading {
		return row(key, model.KnownAmount(v), model.StatusUnmeasured, "")
	}
	tests := []struct {
		name  string
		props string
		rows  []model.LimitReading
		want  []string // the notes of rows, in order
	}{
		{"unlimited vs infinity", "LimitNOFILE=infinity\n", []model.LimitReading{unlimited("rlimit.nofile.hard")},
			[]string{"systemd: LimitNOFILE=infinity on a.service"}},
		{"unlimited vs finite", "LimitNOFILE=4096\n", []model.LimitReading{unlimited("rlimit.nofile.hard")},
			[]string{"systemd: configured LimitNOFILE=4096 on a.service, effective unlimited"}},
		{"finite vs infinity", "LimitNOFILE=infinity\n", []model.LimitReading{finite("rlimit.nofile.hard", 2147483584)},
			[]string{"systemd: configured LimitNOFILE=infinity on a.service, effective 2147483584"}},
		{"finite equal", "LimitNOFILE=4096\n", []model.LimitReading{finite("rlimit.nofile.hard", 4096)},
			[]string{"systemd: LimitNOFILE=4096 on a.service"}},
		{"soft goes to the soft row", "LimitNOFILE=8192\nLimitNOFILESoft=4096\n",
			[]model.LimitReading{finite("rlimit.nofile.soft", 4096), finite("rlimit.nofile.hard", 8192)},
			[]string{"systemd: LimitNOFILESoft=4096 on a.service", "systemd: LimitNOFILE=8192 on a.service"}},
		{"a single row takes both", "LimitNOFILE=4096\nLimitNOFILESoft=4096\n",
			[]model.LimitReading{finite("rlimit.nofile.single", 4096)},
			[]string{"systemd: LimitNOFILE=4096 (soft and hard) on a.service"}},
		{"a single row with a mismatch keeps the notes apart", "LimitNOFILE=4096\nLimitNOFILESoft=4096\n",
			[]model.LimitReading{finite("rlimit.nofile.single", 512)},
			[]string{"systemd: configured LimitNOFILE=4096 on a.service, effective 512; " +
				"systemd: configured LimitNOFILESoft=4096 on a.service, effective 512"}},
		{"a single row with differing directives keeps the notes apart", "LimitNOFILE=4096\nLimitNOFILESoft=1024\n",
			[]model.LimitReading{finite("rlimit.nofile.single", 4096)},
			[]string{"systemd: LimitNOFILE=4096 on a.service; " +
				"systemd: configured LimitNOFILESoft=1024 on a.service, effective 4096"}},
		{"a single row with a bad directive keeps the notes apart", "LimitNOFILE=4096\nLimitNOFILESoft=x\n",
			[]model.LimitReading{finite("rlimit.nofile.single", 4096)},
			[]string{`systemd: LimitNOFILE=4096 on a.service; systemd: LimitNOFILESoft="x" on a.service was not understood`}},
		{"split rows are never merged", "LimitNOFILE=4096\nLimitNOFILESoft=4096\n",
			[]model.LimitReading{finite("rlimit.nofile.soft", 4096), finite("rlimit.nofile.hard", 4096)},
			[]string{"systemd: LimitNOFILESoft=4096 on a.service", "systemd: LimitNOFILE=4096 on a.service"}},
		{"hard directive does not touch the soft row", "LimitNOFILE=4096\n",
			[]model.LimitReading{finite("rlimit.nofile.soft", 1), unlimited("rlimit.nofile.hard")},
			[]string{"", "systemd: configured LimitNOFILE=4096 on a.service, effective unlimited"}},
		{"not understood", "LimitCPU=forever\n", []model.LimitReading{finite("rlimit.cpu.hard", 1)},
			[]string{`systemd: LimitCPU="forever" on a.service was not understood`}},
		{"absent row is skipped", "LimitNOFILE=4096\n", []model.LimitReading{finite("rlimit.nproc.hard", 1)}, []string{""}},
		{"other segment untouched", "LimitNPROC=5\n",
			[]model.LimitReading{finite("rlimit.nofile.hard", 1), finite("rlimit.nproc.hard", 5)},
			[]string{"", "systemd: LimitNPROC=5 on a.service"}},
		{"unparseable row is left alone", "LimitNOFILE=4096\n",
			[]model.LimitReading{row("rlimit.nofile.hard", model.UnknownAmount(), model.StatusUnparseable, "bad")},
			[]string{"bad"}},
		{"unreadable row is left alone", "LimitNOFILE=4096\n",
			[]model.LimitReading{row("rlimit.nofile.hard", model.UnknownAmount(), model.StatusUnreadable, "")},
			[]string{""}},
		{"an existing note is kept", "LimitNOFILE=4096\n",
			[]model.LimitReading{row("rlimit.nofile.hard", model.KnownAmount(4096), model.StatusUnmeasured, "kept")},
			[]string{"kept; systemd: LimitNOFILE=4096 on a.service"}},
		{"no directives at all", "", []model.LimitReading{finite("rlimit.nofile.hard", 1)}, []string{""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			after := Annotate(tc.rows, unitOf(t, tc.props), unitFiles(nil))
			assertOnlyNotesChanged(t, tc.rows, after)
			for i, want := range tc.want {
				if after[i].Notes != want {
					t.Errorf("row %s notes\n  got  %q\n  want %q", after[i].Key, after[i].Notes, want)
				}
			}
		})
	}
}

func TestAnnotateSkipsRowsItCannotTrust(t *testing.T) {
	for _, st := range []model.Status{model.StatusUnparseable, model.StatusUnreadable} {
		before := []model.LimitReading{row("cgroup.pids.max", model.UnknownAmount(), st, "orig")}
		after := Annotate(before, unitOf(t, "TasksMax=7\n"), unitFiles(map[string]string{"pids.max": "7\n"}))
		if after[0].Notes != "orig" {
			t.Errorf("%s row was annotated: %q", st, after[0].Notes)
		}
	}
	// A directive with no row to land on adds nothing, and in particular no row.
	before := []model.LimitReading{row("cgroup.memory.max", model.KnownAmount(1), model.StatusUnmeasured, "")}
	after := Annotate(before, unitOf(t, "TasksMax=7\n"), unitFiles(map[string]string{"pids.max": "7\n"}))
	if len(after) != 1 || after[0].Notes != "" {
		t.Errorf("after = %+v", after)
	}
}

func TestAnnotateDoesNotMutateItsInput(t *testing.T) {
	before := []model.LimitReading{row("cgroup.pids.max", model.KnownAmount(7), model.StatusUnmeasured, "n")}
	_ = Annotate(before, unitOf(t, "TasksMax=7\n"), unitFiles(map[string]string{"pids.max": "7\n"}))
	if before[0].Notes != "n" {
		t.Errorf("input notes changed to %q", before[0].Notes)
	}
}

func TestRoundedDown(t *testing.T) {
	tests := []struct {
		d, f uint64
		want bool
	}{
		{1000001, 999424, true},
		{1000001, 1000001, false},
		{1000001, 1003520, false},
		{4097, 4096, true},
		{8192, 4096, false}, // a whole page too far below
		{100, 0, true},
		{0, 0, false},
	}
	for _, tc := range tests {
		if got := roundedDown(tc.d, tc.f); got != tc.want {
			t.Errorf("roundedDown(%d, %d) = %v, want %v", tc.d, tc.f, got, tc.want)
		}
	}
}

// TestQuotaFlooredAbove64Bits pins the floored comparison where the product
// does not fit in 64 bits, against math/big as the reference.
func TestQuotaFlooredAbove64Bits(t *testing.T) {
	tests := []struct {
		perSecond, period uint64
	}{
		{4_000_000_000_001, 10_000_001},
		{1 << 63, 3_000_001},
		{18446744073709551615, 1_000_000},
		{18446744073709551615, 18446744073709551615}, // quotient does not fit in 64 bits
		{7, 1},
	}
	for _, tc := range tests {
		product := new(big.Int).Mul(new(big.Int).SetUint64(tc.perSecond), new(big.Int).SetUint64(tc.period))
		floor := new(big.Int).Quo(product, big.NewInt(1_000_000))
		fits := floor.IsUint64()
		var quota uint64
		if fits {
			quota = floor.Uint64()
		}
		fl := cgroup.FileLimit{Value: quota, Period: tc.period}
		if got := quotaFloored(tc.perSecond, fl); got != fits {
			t.Errorf("quotaFloored(%d, %+v) = %v, want %v", tc.perSecond, fl, got, fits)
		}
		if fits {
			fl.Value++
			if quotaFloored(tc.perSecond, fl) {
				t.Errorf("quotaFloored accepted %d for a floor of %d", fl.Value, quota)
			}
		}
	}
}
