package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTree builds a --root holding a v2 cgroup /app with the given
// controller files (name to content, under sys/fs/cgroup/app). There is no
// rlimits file, so the rlimit source reports a source_error row: every tree
// built here is a partial report, which is exactly the case --warn must not
// mistake for an all-clear.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	all := map[string]string{
		"proc/4242/cgroup":                 "0::/app\n",
		"sys/fs/cgroup/cgroup.controllers": "cpuset cpu io memory pids\n",
	}
	for name, content := range files {
		all["sys/fs/cgroup/app/"+name] = content
	}
	for name, content := range all {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// headlineOf returns the line after the title.
func headlineOf(t *testing.T, stdout string) string {
	t.Helper()
	lines := strings.Split(stdout, "\n")
	if len(lines) < 3 || lines[0] != "limits-explain" {
		t.Fatalf("unexpected table:\n%s", stdout)
	}
	return lines[1]
}

func TestWarnFlagValidation(t *testing.T) {
	root := cgroupRoot(t, "usage_basic")
	for _, bad := range []string{"", "+90", "90.5", "0", "101", "-1", "0x5a", "9 0", "ninety", "1e2", "٩٠", "18446744073709551616"} {
		for _, form := range []string{"separate", "equals"} {
			t.Run(form+"/"+bad, func(t *testing.T) {
				args := []string{"show", "4242", "--root", root, "--warn", bad}
				if form == "equals" {
					args = []string{"show", "4242", "--root", root, "--warn=" + bad}
				}
				res := run(args...)
				if res.code != 2 {
					t.Errorf("exit %d, want 2 (stderr: %s)", res.code, res.stderr)
				}
				if res.stdout != "" {
					t.Errorf("stdout = %q, want empty", res.stdout)
				}
				if !strings.Contains(res.stderr, "--warn") || !strings.Contains(res.stderr, "Try 'limits-explain --help'") {
					t.Errorf("stderr = %q", res.stderr)
				}
			})
		}
	}
}

func TestWarnFlagAcceptsTheWholeRange(t *testing.T) {
	root := cgroupRoot(t, "usage_basic") // highest reading is 77.5%
	tests := []struct {
		warn string
		code int
	}{
		{"1", 1}, {"9", 1}, {"77", 1}, {"78", 0}, {"90", 0}, {"100", 0},
		// Plain decimal, not octal: 075 is 75, which 77.5% is at or above.
		{"075", 1}, {"090", 0},
	}
	for _, tc := range tests {
		t.Run(tc.warn, func(t *testing.T) {
			if res := run("show", "4242", "--root", root, "--warn", tc.warn); res.code != tc.code {
				t.Errorf("exit %d, want %d (stderr: %s)", res.code, tc.code, res.stderr)
			}
		})
	}
}

func TestDefaultWarnIs90(t *testing.T) {
	tree := func(current string) string {
		return writeTree(t, map[string]string{"memory.max": "10\n", "memory.current": current + "\n"})
	}
	if res := run("show", "4242", "--root", tree("8")); res.code != 0 {
		t.Errorf("80%%: exit %d, want 0", res.code)
	}
	if res := run("show", "4242", "--root", tree("9")); res.code != 1 {
		t.Errorf("90%%: exit %d, want 1", res.code)
	}
}

func TestExitOneOnAMeasuredReadingOverTheLimit(t *testing.T) {
	root := cgroupRoot(t, "usage_over_limit")
	res := run("show", "4242", "--root", root)
	if res.code != 1 {
		t.Fatalf("exit %d, want 1", res.code)
	}
	// The report is already on stdout, ranked, with the warning and the gap.
	lines := strings.Split(res.stdout, "\n")
	wantHead := []string{
		"limits-explain",
		"nearest ceiling: memory 112.5% (1.0 KiB of 1000 B) (1 limit not measured)",
		"warning: 2 readings at or above --warn 90%",
	}
	if !reflect.DeepEqual(lines[:3], wantHead) {
		t.Errorf("head = %q, want %q", lines[:3], wantHead)
	}
	if !strings.Contains(res.stdout, "over by 125") {
		t.Errorf("no over-by cell:\n%s", res.stdout)
	}
	// One line on stderr, and no usage hint: this is not a usage error.
	if res.stderr != "limits-explain: 2 readings at or above --warn 90%\n" {
		t.Errorf("stderr = %q", res.stderr)
	}
}

func TestExitOneAppliesToJSONAndKeepsRankedOrder(t *testing.T) {
	res := run("show", "4242", "--root", cgroupRoot(t, "usage_over_limit"), "--json")
	if res.code != 1 {
		t.Fatalf("exit %d, want 1", res.code)
	}
	rows := decodeJSON(t, res.stdout) // still exactly one bare array
	var order []string
	for _, row := range rows {
		order = append(order, row["key"].(string))
	}
	want := []string{"cgroup.memory.max", "cgroup.pids.max", "rlimit.source_error"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	over := rows[0]
	if over["headroom"].(json.Number).String() != "0" || over["over_by"].(json.Number).String() != "125" {
		t.Errorf("headroom/over_by = %v/%v, want 0/125", over["headroom"], over["over_by"])
	}
	full := rows[1]
	if full["headroom"].(json.Number).String() != "0" || full["over_by"].(json.Number).String() != "0" {
		t.Errorf("exactly full: headroom/over_by = %v/%v, want 0/0", full["headroom"], full["over_by"])
	}
	if gap := rows[2]; gap["headroom"] != nil || gap["over_by"] != nil {
		t.Errorf("unmeasured row: headroom/over_by = %v/%v, want null", gap["headroom"], gap["over_by"])
	}
}

func TestPartialReportUnderTheThresholdExitsZeroAndNamesTheGap(t *testing.T) {
	// usage_basic: a rlimit source_error row (a gap) plus measured readings
	// at 77.5%, 62% and 9%. Nothing measured is over, so exit 0, but the
	// headline must not read as an all-clear.
	res := run("show", "4242", "--root", cgroupRoot(t, "usage_basic"))
	if res.code != 0 || res.stderr != "" {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	want := "nearest ceiling: memory (high) 77.5% (620 B of 800 B) (1 limit not measured)"
	if got := headlineOf(t, res.stdout); got != want {
		t.Errorf("headline = %q, want %q", got, want)
	}
	if strings.Contains(res.stdout, "warning:") {
		t.Errorf("unexpected warning line:\n%s", res.stdout)
	}
	// Ranked: nearest ceiling first, then the gap, then the unlimited row.
	order := []string{"memory (high)", "memory  ", "tasks", "rlimit source", "cpu bandwidth"}
	last := -1
	for _, name := range order {
		i := strings.Index(res.stdout, "\n"+name)
		if i <= last {
			t.Fatalf("%q out of order:\n%s", name, res.stdout)
		}
		last = i
	}
}

func TestNothingMeasuredIsNotAnAllClear(t *testing.T) {
	// Every cgroup reading is unlimited and the rlimit source failed: exit 0,
	// and the headline says unknown, not "none".
	res := run("show", "4242", "--root", cgroupRoot(t, "all_max"))
	if res.code != 0 {
		t.Fatalf("exit %d", res.code)
	}
	if got, want := headlineOf(t, res.stdout), "nearest ceiling: unknown (1 limit not measured)"; got != want {
		t.Errorf("headline = %q, want %q", got, want)
	}
}

func TestUsageReadAtTheAncestorDecidesTheExit(t *testing.T) {
	// The ancestor is at 90% (memory) while the leaf's own counter says 2%:
	// the exit follows the binding level.
	res := run("show", "4242", "--root", cgroupRoot(t, "usage_ancestor_binds"))
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", res.code, res.stdout)
	}
	if got, want := headlineOf(t, res.stdout), "nearest ceiling: memory 90.0% (900 B of 1000 B) (1 limit not measured)"; got != want {
		t.Errorf("headline = %q, want %q", got, want)
	}
}

func TestThresholdIsExactNotFloat(t *testing.T) {
	tests := []struct {
		name     string
		max, cur string
		warn     string
		code     int
		pct      string
	}{
		{"exactly 90 percent warns", "10", "9", "90", 1, "90.0%"},
		{"89.99999 percent does not warn and shows 89.9", "10000000", "8999999", "90", 0, "89.9%"},
		// float64 reads 2^60 / (2^60+1) as exactly 1.0 and would warn at 100.
		{"2^60 one short of the limit", "1152921504606846977", "1152921504606846976", "100", 0, "99.9%"},
		{"2^60 at the limit", "1152921504606846977", "1152921504606846977", "100", 1, "100.0%"},
		{"max uint64 minus one at 99", "18446744073709551615", "18446744073709551614", "99", 1, "99.9%"},
		{"max uint64 minus one at 100", "18446744073709551615", "18446744073709551614", "100", 0, "99.9%"},
		{"zero limit with zero usage", "0", "0", "100", 1, "100.0%"},
		{"zero limit with usage", "0", "5", "1", 1, "100.0%"},
		{"usage over the limit", "100", "18446744073709551615", "100", 1, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, map[string]string{"memory.max": tc.max + "\n", "memory.current": tc.cur + "\n"})
			res := run("show", "4242", "--root", root, "--warn", tc.warn)
			if res.code != tc.code {
				t.Errorf("exit %d, want %d\n%s%s", res.code, tc.code, res.stdout, res.stderr)
			}
			if tc.pct != "" && !strings.Contains(headlineOf(t, res.stdout), " "+tc.pct+" (") {
				t.Errorf("headline = %q, want it to show %s", headlineOf(t, res.stdout), tc.pct)
			}
		})
	}
}

func TestUsageOverLimitNeverWrapsAround(t *testing.T) {
	root := writeTree(t, map[string]string{"memory.max": "100\n", "memory.current": "18446744073709551615\n"})
	res := run("show", "4242", "--root", root, "--json")
	rows := rowsByKey(t, res.stdout)
	row := rows["cgroup.memory.max"]
	if row["headroom"].(json.Number).String() != "0" ||
		row["over_by"].(json.Number).String() != "18446744073709551515" {
		t.Errorf("headroom/over_by = %v/%v", row["headroom"], row["over_by"])
	}
	if res.code != 1 {
		t.Errorf("exit %d, want 1", res.code)
	}
}

func TestRenderFailureBeatsTheThreshold(t *testing.T) {
	// Over the threshold AND the write fails: 4, not 1, and no threshold
	// message that would suggest the report was delivered.
	for _, args := range [][]string{
		{"show", "4242", "--root", cgroupRoot(t, "usage_over_limit")},
		{"show", "4242", "--root", cgroupRoot(t, "usage_over_limit"), "--json"},
	} {
		var stderr bytes.Buffer
		if code := Execute(args, brokenWriter{}, &stderr); code != 4 {
			t.Errorf("%v: exit %d, want 4", args, code)
		}
		if strings.Contains(stderr.String(), "--warn") || !strings.Contains(stderr.String(), "broken pipe") {
			t.Errorf("%v: stderr = %q", args, stderr.String())
		}
	}
}

func TestExitThreeStillWinsWithNothingToReport(t *testing.T) {
	res := run("show", "9999", "--root", cgroupRoot(t, "usage_over_limit"))
	if res.code != 3 || res.stdout != "" {
		t.Errorf("exit %d, stdout %q, want 3 and empty", res.code, res.stdout)
	}
}

func TestHelpDocumentsWarnAndExitOne(t *testing.T) {
	res := run("show", "--help")
	for _, want := range []string{"--warn PERCENT", "at least one MEASURED reading is at or above --warn", "an all-clear by itself"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("help lacks %q:\n%s", want, res.stdout)
		}
	}
	for _, stale := range []string{"reserved", "never returned"} {
		if strings.Contains(res.stdout, stale) {
			t.Errorf("help still says %q:\n%s", stale, res.stdout)
		}
	}
}

func TestParseWarn(t *testing.T) {
	for in, want := range map[string]uint64{"1": 1, "90": 90, "100": 100, "007": 7} {
		if got, err := parseWarn(in); err != nil || got != want {
			t.Errorf("parseWarn(%q) = %d, %v, want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "000", "101", "1000", "+1", "-1", "1.0", " 5", "5 ", "0b11", "0o7"} {
		if _, err := parseWarn(in); err == nil {
			t.Errorf("parseWarn(%q) succeeded, want an error", in)
		}
	}
}
