package systemd

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mabunemeh/limits-explain/internal/model"
	"github.com/mabunemeh/limits-explain/internal/sources"
	"github.com/mabunemeh/limits-explain/internal/sources/cgroup"
	"github.com/mabunemeh/limits-explain/internal/sources/rlimits"
)

// fixtureDir returns the committed fixture tree for a case.
func fixtureDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join("testdata", name)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("no such fixture tree: %s (%v)", dir, err)
	}
	return dir
}

// replay is a Runner that returns a fixed answer and records how it was asked.
type replay struct {
	out    string
	err    error
	units  []string
	props  [][]string
	called int
}

func (r *replay) Show(unit string, props []string) (string, error) {
	r.called++
	r.units = append(r.units, unit)
	r.props = append(r.props, props)
	return r.out, r.err
}

// replayFile replays the byte-exact show.txt of a fixture case.
func replayFile(t *testing.T, name string) *replay {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir(t, name), "show.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return &replay{out: string(b)}
}

// collectAll runs the rlimit and cgroup sources over a fixture, as the CLI does
// before the systemd step.
func collectAll(t *testing.T, fsys fs.FS) []model.LimitReading {
	t.Helper()
	var all []model.LimitReading
	for _, src := range []sources.Source{rlimits.New(), cgroup.New()} {
		got, err := src.Collect(4242, fsys)
		if err != nil {
			t.Fatalf("%s: %v", src.Name(), err)
		}
		all = append(all, got...)
	}
	return all
}

func TestValidateUnitName(t *testing.T) {
	ok := []string{
		"a.service", "getty@tty1.service", `foo\x2dbar.service`, "a:b_c-d.scope", "nginx", "A0",
		strings.Repeat("a", 255),
	}
	for _, name := range ok {
		if err := ValidateUnitName(name); err != nil {
			t.Errorf("ValidateUnitName(%q) = %v, want nil", name, err)
		}
	}
	bad := []string{
		"", ".", "..", "-x", "--help", "-", "foo bar", "a/b", "a\nb", "a\tb", "a=b", "a,b", "a*", "a;b", "a$b",
		"a\xc3\xbc.service", "a\x00b", strings.Repeat("a", 256),
	}
	for _, name := range bad {
		if err := ValidateUnitName(name); err == nil {
			t.Errorf("ValidateUnitName(%q) = nil, want an error", name)
		}
	}
}

func TestParseNumber(t *testing.T) {
	tests := []struct {
		raw   string
		state numState
		value uint64
	}{
		{"7", stateFinite, 7},
		{"0", stateFinite, 0},
		{"18446744073709551615", stateFinite, 18446744073709551615},
		{"infinity", stateInfinite, 0},
		{"", stateBad, 0},
		{"-1", stateBad, 0},
		{"+1", stateBad, 0},
		{"0x10", stateBad, 0},
		{"1e3", stateBad, 0},
		{"1_0", stateBad, 0},
		{"1.5", stateBad, 0},
		{" 7", stateBad, 0},
		{"7 ", stateBad, 0},
		{"Infinity", stateBad, 0},
		{"18446744073709551616", stateBad, 0},
		{"\xd9\xa3", stateBad, 0}, // an Arabic-Indic digit is not an ASCII digit
	}
	for _, tc := range tests {
		got := parseNumber(tc.raw)
		if got.state != tc.state || got.value != tc.value || got.raw != tc.raw {
			t.Errorf("parseNumber(%q) = %+v, want state %d value %d", tc.raw, got, tc.state, tc.value)
		}
	}
}

func TestParseTimespan(t *testing.T) {
	good := []struct {
		in   string
		want uint64
	}{
		{"75ms", 75_000},
		{"1.500000s", 1_500_000},
		{"500ms", 500_000},
		{"1s", 1_000_000},
		{"7us", 7},
		{"1min", 60_000_000},
		{"1h", 3_600_000_000},
		{"1min 30s", 90_000_000},
		{"1h 1min 1s 1ms 1us", 3_661_001_001},
		{"0.000001s", 1},
		{"0.001ms", 1},
		{"2.5ms", 2_500},
		{"0s", 0},
		{"18446744073709551615us", 18446744073709551615},
		{"007ms", 7_000},
	}
	for _, tc := range good {
		got, ok := parseTimespan(tc.in)
		if !ok || got != tc.want {
			t.Errorf("parseTimespan(%q) = %d, %v; want %d, true", tc.in, got, ok, tc.want)
		}
	}
	bad := []string{
		"", "0", "5", "s", "ms", "1.5us", "1.0000005s", "1e3ms", "-5ms", "+5ms", "1 s", "1.s", ".5s", "1..5s",
		" 75ms", "75ms ", "75ms  1s", "1d", "1w", "1 ms", "0x10ms", "1_0ms", "infinity", "Ms", "1S",
		"18446744073709551616us", "18446744073709551615us 1us", "5000000000000h",
		"\xd9\xa3s", "1.5\xd9\xa3s", strings.Repeat("9", 41) + "us", "1." + strings.Repeat("0", 41) + "s",
	}
	for _, in := range bad {
		if got, ok := parseTimespan(in); ok {
			t.Errorf("parseTimespan(%q) = %d, true; want a rejection", in, got)
		}
	}
}

func TestParseQuota(t *testing.T) {
	if d := parseQuota("infinity"); d.state != stateInfinite {
		t.Errorf("infinity -> %+v", d)
	}
	if d := parseQuota("75ms"); d.state != stateFinite || d.value != 75_000 || d.raw != "75ms" {
		t.Errorf("75ms -> %+v", d)
	}
	if d := parseQuota("1.5us"); d.state != stateBad || d.raw != "1.5us" {
		t.Errorf("1.5us -> %+v", d)
	}
}

func TestParseShow(t *testing.T) {
	got, err := parseShow("A=1\nB=\nC=x=y\n")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "", "C": "x=y"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseShow = %v, want %v", got, want)
	}
	if got, err := parseShow("A=1\r\nB=2\r\n"); err != nil || got["B"] != "2" {
		t.Errorf("CRLF output: %v, %v", got, err)
	}
	if got, err := parseShow("A=1"); err != nil || got["A"] != "1" {
		t.Errorf("no trailing newline: %v, %v", got, err)
	}
	if got, err := parseShow(""); err != nil || len(got) != 0 {
		t.Errorf("empty output: %v, %v", got, err)
	}
	for _, bad := range []string{"A=1\nnoequals\n", "A=1\n\nB=2\n", "=1\n", "A=1\nA=2\n", "A=1\n\n\n", "\n"} {
		if _, err := parseShow(bad); err == nil {
			t.Errorf("parseShow(%q) = nil error, want a structural error", bad)
		}
	}
}

func TestProperties(t *testing.T) {
	props := properties()
	seen := map[string]bool{}
	for _, p := range props {
		if seen[p] {
			t.Errorf("property %s asked twice", p)
		}
		seen[p] = true
		if !validProperty(p) {
			t.Errorf("property %q would be refused by the runner", p)
		}
	}
	for _, want := range []string{"Id", "LoadState", "ActiveState", "MainPID", "ControlGroup", "TasksMax",
		"MemoryMax", "MemoryHigh", "CPUQuotaPerSecUSec", "CPUQuotaPeriodUSec", "LimitNOFILE", "LimitNOFILESoft",
		"LimitRTTIME", "LimitRTTIMESoft"} {
		if !seen[want] {
			t.Errorf("property %s is not requested", want)
		}
	}
	if len(props) != 10+2*16 {
		t.Errorf("%d properties, want %d", len(props), 10+2*16)
	}
}

// TestRlimitSegmentsMatchTheRlimitSource pins the Limit<X> table to the key
// segments the rlimits source really produces, from a real 16-row limits file.
func TestRlimitSegmentsMatchTheRlimitSource(t *testing.T) {
	rows := collectAll(t, os.DirFS(fixtureDir(t, "all_match")))
	got := map[string]bool{}
	for _, r := range rows {
		if strings.HasPrefix(r.Key, "rlimit.") {
			got[strings.Split(r.Key, ".")[1]] = true
		}
	}
	var have, want []string
	for seg := range got {
		have = append(have, seg)
	}
	for _, l := range rlimitProps {
		want = append(want, l.segment)
	}
	sort.Strings(have)
	sort.Strings(want)
	if !reflect.DeepEqual(have, want) {
		t.Errorf("rlimit segments\n  source: %v\n  table:  %v", have, want)
	}
}

// TestFixturesAnswerEveryRequestedProperty checks the captured show output
// against what the code asks for: a property the code requests but a real
// systemd never printed would be a typo.
func TestFixturesAnswerEveryRequestedProperty(t *testing.T) {
	for _, name := range []string{"all_match", "nofile_infinity", "cpu_fraction", "cpu_150", "cpu_floored", "rlimit_changed",
		"pids_overwritten", "memory_rounded", "not_found"} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(fixtureDir(t, name), "show.txt"))
			if err != nil {
				t.Fatal(err)
			}
			props, err := parseShow(string(b))
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range properties() {
				if _, ok := props[p]; !ok {
					t.Errorf("%s is requested but absent from the capture", p)
				}
			}
			if len(props) != len(properties()) {
				t.Errorf("capture has %d properties, the code asks for %d", len(props), len(properties()))
			}
		})
	}
	// Scopes have no MainPID line and no Limit lines at all.
	b, err := os.ReadFile(filepath.Join(fixtureDir(t, "scope"), "show.txt"))
	if err != nil {
		t.Fatal(err)
	}
	props, err := parseShow(string(b))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"MainPID", "LimitNOFILE"} {
		if _, ok := props[p]; ok {
			t.Errorf("scope capture unexpectedly has %s", p)
		}
	}
	if u, err := newUnit(props); err != nil || u.MainPID != 0 || !u.Loaded() {
		t.Errorf("scope unit = %+v, %v", u, err)
	}
}

func TestNewUnitRejectsStructuralProblems(t *testing.T) {
	tests := []struct {
		name  string
		props map[string]string
		want  string
	}{
		{"no LoadState", map[string]string{"Id": "a.service"}, "LoadState is missing"},
		{"MainPID text", map[string]string{"LoadState": "loaded", "MainPID": "x"}, "MainPID is not a number"},
		{"MainPID negative", map[string]string{"LoadState": "loaded", "MainPID": "-1"}, "MainPID is not a number"},
		{"MainPID huge", map[string]string{"LoadState": "loaded", "MainPID": "99999999999"}, "out of range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newUnit(tc.props); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name   string
		files  map[string]string
		unit   string
		reason string
	}{
		{"service", map[string]string{"proc/4242/cgroup": "0::/system.slice/a.service\n"}, "a.service", ""},
		{"scope", map[string]string{"proc/4242/cgroup": "0::/user.slice/user-1000.slice/session-3.scope\n"},
			"session-3.scope", ""},
		{"rootmost wins", map[string]string{"proc/4242/cgroup": "0::/system.slice/a.service/b.scope\n"},
			"a.service", ""},
		{"rootmost scope over service", map[string]string{"proc/4242/cgroup": "0::/machine.slice/x.scope/y.service\n"},
			"x.scope", ""},
		{"sub-cgroup of a service", map[string]string{"proc/4242/cgroup": "0::/system.slice/a.service/payload\n"},
			"a.service", ""},
		{"CRLF file", map[string]string{"proc/4242/cgroup": "0::/system.slice/a.service\r\n"}, "a.service", ""},
		{"user manager", map[string]string{
			"proc/4242/cgroup": "0::/user.slice/user-1000.slice/user@1000.service/app.slice/x.scope\n"},
			"", reasonUserManager},
		{"user manager itself", map[string]string{
			"proc/4242/cgroup": "0::/user.slice/user-1000.slice/user@1000.service\n"}, "", reasonUserManager},
		{"user@ without digits is just a unit", map[string]string{
			"proc/4242/cgroup": "0::/system.slice/user@abc.service\n"}, "user@abc.service", ""},
		{"non-systemd", map[string]string{"proc/4242/cgroup": "0::/non-systemd\n"}, "", reasonNotInUnit},
		{"root cgroup", map[string]string{"proc/4242/cgroup": "0::/\n"}, "", reasonNotInUnit},
		{"slice only", map[string]string{"proc/4242/cgroup": "0::/system.slice\n"}, "", reasonNotInUnit},
		{"suffix with no name", map[string]string{"proc/4242/cgroup": "0::/system.slice/.service\n"},
			"", reasonNotInUnit},
		{"invalid unit name", map[string]string{"proc/4242/cgroup": "0::/system.slice/a b.service\n"},
			"", reasonBadUnitName},
		{"cgroup file missing", map[string]string{}, "", reasonCgroupRead},
		{"v1 only", map[string]string{"proc/4242/cgroup": "1:name=systemd:/system.slice/a.service\n"},
			"", reasonNoUnified},
		{"two unified lines", map[string]string{
			"proc/4242/cgroup": "0::/system.slice/a.service\n0::/system.slice/b.service\n"}, "", reasonNoUnified},
		{"relative path", map[string]string{"proc/4242/cgroup": "0::system.slice/a.service\n"}, "", reasonNoUnified},
		{"dot-dot", map[string]string{"proc/4242/cgroup": "0::/system.slice/../a.service\n"}, "", reasonNoUnified},
		{"empty file", map[string]string{"proc/4242/cgroup": ""}, "", reasonNoUnified},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := fstest.MapFS{"proc/1/comm": {Data: []byte("systemd\n")}}
			for name, data := range tc.files {
				m[name] = &fstest.MapFile{Data: []byte(data)}
			}
			got := resolve(4242, m)
			if got.unit != tc.unit || got.reason != tc.reason {
				t.Errorf("resolve = %+v, want unit %q reason %q", got, tc.unit, tc.reason)
			}
		})
	}
}

func TestResolveNeedsSystemdAsPID1(t *testing.T) {
	cg := &fstest.MapFile{Data: []byte("0::/system.slice/a.service\n")}
	tests := []struct {
		name   string
		comm   *fstest.MapFile
		reason string
	}{
		{"init", &fstest.MapFile{Data: []byte("init\n")}, reasonPID1NotSystemd},
		{"almost", &fstest.MapFile{Data: []byte("systemd-x\n")}, reasonPID1NotSystemd},
		{"empty", &fstest.MapFile{Data: []byte("")}, reasonPID1NotSystemd},
		{"missing", nil, reasonPID1Unreadable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := fstest.MapFS{"proc/4242/cgroup": cg}
			if tc.comm != nil {
				m["proc/1/comm"] = tc.comm
			}
			if got := resolve(4242, m); got.unit != "" || got.reason != tc.reason {
				t.Errorf("resolve = %+v, want reason %q", got, tc.reason)
			}
		})
	}
}

func TestOwnsCgroup(t *testing.T) {
	tests := []struct {
		cg, pid string
		want    bool
	}{
		{"/system.slice/a.service", "/system.slice/a.service", true},
		{"/system.slice/a.service", "/system.slice/a.service/payload/x", true},
		{"/system.slice/a.service", "/system.slice/a.service2", false},
		{"/system.slice/a.service", "/system.slice", false},
		{"/system.slice/a.service", "/system.slice/b.service", false},
		{"/system.slice/a.service/", "/system.slice/a.service", true},
		{"", "/system.slice/a.service", false},
		{"/", "/system.slice/a.service", false},
		{"system.slice/a.service", "/system.slice/a.service", false},
		{"/system.slice/../a.service", "/system.slice/a.service", false},
		{"/system.slice/a.service", "relative", false},
	}
	for _, tc := range tests {
		if got := ownsCgroup(tc.cg, tc.pid); got != tc.want {
			t.Errorf("ownsCgroup(%q, %q) = %v, want %v", tc.cg, tc.pid, got, tc.want)
		}
	}
}

func TestOutcomeLine(t *testing.T) {
	tests := []struct {
		o    Outcome
		want string
	}{
		{Outcome{Unit: "a.service"}, "unit: a.service"},
		{Outcome{Unit: "a.service", Failed: true}, "unit: a.service (directives not read)"},
		{Outcome{Reason: reasonNotInUnit}, "unit: none (not in a systemd unit)"},
		{Outcome{Reason: ReasonNotConsulted}, "unit: none (not consulted under --root)"},
	}
	for _, tc := range tests {
		if got := tc.o.Line(); got != tc.want {
			t.Errorf("Line() = %q, want %q", got, tc.want)
		}
	}
}

// consult runs the whole systemd step over a committed fixture.
func consult(t *testing.T, name string, r Runner) ([]model.LimitReading, []model.LimitReading, Outcome, error) {
	t.Helper()
	fsys := os.DirFS(fixtureDir(t, name))
	before := collectAll(t, fsys)
	after, out, err := Consult(r, 4242, fsys, before)
	return before, after, out, err
}

func TestConsultAnnotatesARealUnit(t *testing.T) {
	r := replayFile(t, "all_match")
	before, after, out, err := consult(t, "all_match", r)
	if err != nil {
		t.Fatal(err)
	}
	if out != (Outcome{Unit: "le-match.service"}) {
		t.Errorf("outcome = %+v", out)
	}
	if r.called != 1 || r.units[0] != "le-match.service" || !reflect.DeepEqual(r.props[0], properties()) {
		t.Errorf("runner asked %d times about %v with %v", r.called, r.units, r.props)
	}
	assertOnlyNotesChanged(t, before, after)
	annotated := 0
	for i := range after {
		if after[i].Notes != before[i].Notes {
			annotated++
		}
	}
	if annotated == 0 {
		t.Error("no row was annotated")
	}
}

func TestConsultIsQuietWhenThereIsNoUnit(t *testing.T) {
	tests := []struct {
		fixture string
		reason  string
	}{
		{"synthetic_user_manager", reasonUserManager},
		{"synthetic_pid1_not_systemd", reasonPID1NotSystemd},
		{"non_systemd", reasonNotInUnit},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			r := &replay{err: errors.New("must not be asked")}
			fsys := os.DirFS(fixtureDir(t, tc.fixture))
			in := []model.LimitReading{{Key: "x", Notes: "n"}}
			got, out, err := Consult(r, 4242, fsys, in)
			if err != nil {
				t.Fatalf("a missing unit is not an error: %v", err)
			}
			if out.Unit != "" || out.Reason != tc.reason || out.Failed {
				t.Errorf("outcome = %+v, want reason %q", out, tc.reason)
			}
			if r.called != 0 {
				t.Errorf("systemctl was run %d times", r.called)
			}
			if !reflect.DeepEqual(got, in) {
				t.Errorf("readings changed: %+v", got)
			}
		})
	}
}

func TestConsultWithoutARunner(t *testing.T) {
	in := []model.LimitReading{{Key: "x"}}
	got, out, err := Consult(nil, 4242, os.DirFS(fixtureDir(t, "all_match")), in)
	if err != nil || out.Reason != ReasonNotConsulted || out.Unit != "" || !reflect.DeepEqual(got, in) {
		t.Errorf("got %+v, %+v, %v", got, out, err)
	}
}

func TestConsultFailuresAreSourceErrors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name     string
		fixture  string
		runner   func(t *testing.T) *replay
		want     string
		wrapsErr bool // read failure (Err set, unreadable) rather than a parse failure
	}{
		{"systemctl fails", "all_match", func(*testing.T) *replay { return &replay{err: boom} }, "failed: boom", true},
		{"garbage line", "synthetic_garbage_line", func(t *testing.T) *replay { return replayFile(t, "synthetic_garbage_line") },
			"unparseable output: line 10 is not Key=Value", false},
		{"duplicate key", "synthetic_duplicate_key", func(t *testing.T) *replay { return replayFile(t, "synthetic_duplicate_key") },
			`repeats the key "TasksMax"`, false},
		{"control group elsewhere", "synthetic_controlgroup_elsewhere",
			func(t *testing.T) *replay { return replayFile(t, "synthetic_controlgroup_elsewhere") },
			"neither the process cgroup", false},
		{"empty output", "all_match", func(*testing.T) *replay { return &replay{} }, "LoadState is missing", false},
		{"not loaded", "all_match", func(t *testing.T) *replay { return replayFile(t, "not_found") },
			`LoadState="not-found", expected loaded`, false},
		{"other unit", "all_match", func(*testing.T) *replay {
			return &replay{out: "Id=other.service\nLoadState=loaded\nControlGroup=/system.slice/le-match.service\n"}
		}, `Id="other.service" does not match`, false},
		{"bad MainPID", "all_match", func(*testing.T) *replay {
			return &replay{out: "LoadState=loaded\nMainPID=x\n"}
		}, "MainPID is not a number", false},
		{"empty ControlGroup", "all_match", func(*testing.T) *replay {
			return &replay{out: "Id=le-match.service\nLoadState=loaded\nControlGroup=\n"}
		}, "neither the process cgroup", false},
		{"root ControlGroup", "all_match", func(*testing.T) *replay {
			return &replay{out: "Id=le-match.service\nLoadState=loaded\nControlGroup=/\n"}
		}, "neither the process cgroup", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before, after, out, err := consult(t, tc.fixture, tc.runner(t))
			var se *sources.SourceError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want a *SourceError", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
			if tc.wrapsErr != (se.Err != nil) {
				t.Errorf("SourceError.Err = %v, wrapsErr = %v", se.Err, tc.wrapsErr)
			}
			if tc.wrapsErr && !errors.Is(err, boom) {
				t.Errorf("err does not wrap the runner error: %v", err)
			}
			if !out.Failed || out.Unit != "le-match.service" {
				t.Errorf("outcome = %+v", out)
			}
			if !reflect.DeepEqual(before, after) {
				t.Error("readings changed on a failure")
			}
		})
	}
}

func TestMainPID(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name    string
		fixture string
		runner  Runner
		pid     int
		err     string
	}{
		{"running service", "all_match", nil, 3535, ""},
		{"not found", "not_found", nil, 0, "unit le-missing.service not found"},
		{"scope has no main process", "scope", nil, 0, "unit le-missing.service has no main process"},
		{"main pid 0", "all_match", &replay{out: "LoadState=loaded\nMainPID=0\n"}, 0, "has no main process"},
		{"main pid absent", "all_match", &replay{out: "LoadState=loaded\n"}, 0, "has no main process"},
		{"runner fails", "all_match", &replay{err: boom}, 0, "failed: boom"},
		{"garbage", "all_match", &replay{out: "nonsense\n"}, 0, "unparseable output"},
		{"no systemd", "synthetic_pid1_not_systemd", &replay{}, 0, "systemd is not available: PID 1 is not systemd"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.runner
			if r == nil {
				r = replayFile(t, tc.fixture)
			}
			fsys := os.DirFS(fixtureDir(t, tc.fixture))
			pid, err := MainPID(r, fsys, "le-missing.service")
			switch {
			case tc.err == "" && err != nil:
				t.Fatalf("err = %v", err)
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Fatalf("err = %v, want it to contain %q", err, tc.err)
			}
			if pid != tc.pid {
				t.Errorf("pid = %d, want %d", pid, tc.pid)
			}
		})
	}
	if _, err := MainPID(nil, fstest.MapFS{}, "a.service"); err == nil ||
		!strings.Contains(err.Error(), ReasonNotConsulted) {
		t.Errorf("nil runner: %v", err)
	}
	// proc/1/comm missing altogether.
	if _, err := MainPID(&replay{}, fstest.MapFS{}, "a.service"); err == nil ||
		!strings.Contains(err.Error(), reasonPID1Unreadable) {
		t.Errorf("no PID 1 name: %v", err)
	}
}

func TestQueryAsksForExactlyTheKnownProperties(t *testing.T) {
	r := replayFile(t, "all_match")
	u, err := Query(r, "le-match.service")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "le-match.service" || u.MainPID != 3535 || u.ControlGroup != "/system.slice/le-match.service" ||
		u.ActiveState != "active" || !u.Loaded() {
		t.Errorf("unit = %+v", u)
	}
	if len(u.limits) != 32 {
		t.Errorf("%d Limit directives, want 32", len(u.limits))
	}
	if d := u.limits["LimitNOFILE"]; d.state != stateFinite || d.value != 8192 {
		t.Errorf("LimitNOFILE = %+v", d)
	}
	if d := u.cpuQuota; d.state != stateFinite || d.value != 500_000 {
		t.Errorf("cpu quota = %+v", d)
	}
}
