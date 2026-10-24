package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/mabunemeh/limits-explain/internal/sources/systemd"
)

// systemdRoot returns the committed systemd fixture tree for a case. These are
// full roots: proc/4242, sys/fs/cgroup and the show.txt the fake runner replays.
func systemdRoot(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join("..", "sources", "systemd", "testdata", name)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("no such fixture tree: %s (%v)", root, err)
	}
	return root
}

// replayRunner replays one fixed answer and records every unit it was asked about.
type replayRunner struct {
	out   string
	err   error
	asked []string
}

func (r *replayRunner) Show(unit string, _ []string) (string, error) {
	r.asked = append(r.asked, unit)
	return r.out, r.err
}

// replayOf replays the byte-exact show.txt of a fixture case.
func replayOf(t *testing.T, name string) *replayRunner {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(systemdRoot(t, name), "show.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return &replayRunner{out: string(b)}
}

// runWithRunner runs the CLI with runner injected for every root. A nil
// runner means systemd is not consulted.
func runWithRunner(runner systemd.Runner, args ...string) result {
	var stdout, stderr bytes.Buffer
	code := execute(args, &stdout, &stderr,
		func(root string) fs.FS { return os.DirFS(root) },
		func(string) systemd.Runner { return runner })
	return result{code, stdout.String(), stderr.String()}
}

// unitRoot copies a fixture and renames its proc/4242 to the MainPID its
// show.txt reports, so `show --unit` finds the process the way a real host
// would. The captured files are otherwise untouched.
func unitRoot(t *testing.T, name string) string {
	t.Helper()
	show, err := os.ReadFile(filepath.Join(systemdRoot(t, name), "show.txt"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^MainPID=(\d+)\r?$`).FindSubmatch(show)
	if m == nil {
		t.Fatalf("%s/show.txt has no MainPID", name)
	}
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(systemdRoot(t, name))); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dst, "proc", "4242"), filepath.Join(dst, "proc", string(m[1]))); err != nil {
		t.Fatal(err)
	}
	return dst
}

func lines(s string) []string { return strings.Split(s, "\n") }

func TestShowPidNamesTheUnitAndAnnotates(t *testing.T) {
	r := replayOf(t, "all_match")
	res := runWithRunner(r, "show", "4242", "--root", systemdRoot(t, "all_match"))
	if res.code != 0 {
		t.Fatalf("exit %d, stderr: %s\n%s", res.code, res.stderr, res.stdout)
	}
	got := lines(res.stdout)
	if got[0] != "limits-explain" || !strings.HasPrefix(got[1], "nearest ceiling:") || got[2] != "unit: le-match.service" {
		t.Errorf("head = %q", got[:4])
	}
	for _, want := range []string{
		"systemd: TasksMax=7 on le-match.service",
		"systemd: MemoryMax=67108864 on le-match.service",
		"systemd: LimitNOFILESoft=4096 on le-match.service",
		"systemd: CPUQuotaPerSecUSec=500ms on le-match.service",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	if res.stderr != "" {
		t.Errorf("stderr = %q", res.stderr)
	}
	if len(r.asked) != 1 || r.asked[0] != "le-match.service" {
		t.Errorf("systemctl asked about %v, want one question about the cgroup's unit", r.asked)
	}
}

// TestAnnotationsChangeNoRows checks the contract that systemd only adds
// notes: the same rows, in the same order, with the same numbers and statuses,
// whether or not systemd is consulted, and the JSON stays a bare array.
func TestAnnotationsChangeNoRows(t *testing.T) {
	root := systemdRoot(t, "pids_overwritten")
	plain := runWithRunner(nil, "show", "4242", "--json", "--root", root)
	with := runWithRunner(replayOf(t, "pids_overwritten"), "show", "4242", "--json", "--root", root)
	if plain.code != with.code {
		t.Fatalf("exit %d without systemd, %d with it", plain.code, with.code)
	}
	a, b := decodeJSON(t, plain.stdout), decodeJSON(t, with.stdout)
	if len(a) != len(b) {
		t.Fatalf("%d rows became %d", len(a), len(b))
	}
	annotated := 0
	for i := range a {
		if len(a[i]) != len(b[i]) {
			t.Errorf("row %d changed shape: %v vs %v", i, a[i], b[i])
		}
		for k, v := range a[i] {
			if k == "notes" {
				continue
			}
			if b[i][k] != v {
				t.Errorf("row %v: %s changed from %v to %v", a[i]["key"], k, v, b[i][k])
			}
		}
		if b[i]["notes"] != a[i]["notes"] {
			annotated++
			if !strings.HasPrefix(b[i]["notes"].(string), a[i]["notes"].(string)) {
				t.Errorf("row %v lost its notes", a[i]["key"])
			}
		}
		if b[i]["key"] == "cgroup.pids.max" &&
			!strings.Contains(b[i]["notes"].(string), "systemd: configured TasksMax=7 on le-pids.service, effective 5") {
			t.Errorf("pids.max notes = %v", b[i]["notes"])
		}
	}
	if annotated == 0 {
		t.Error("nothing was annotated")
	}
	if strings.Contains(with.stdout, "unit: ") {
		t.Errorf("the unit line leaked into the JSON:\n%s", with.stdout)
	}
}

func TestWithoutARunnerTheTableSaysSystemdWasNotConsulted(t *testing.T) {
	res := run("show", "4242", "--root", systemdRoot(t, "all_match"))
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if got := lines(res.stdout)[2]; got != "unit: none (not consulted under --root)" {
		t.Errorf("context line = %q", got)
	}
	if strings.Contains(res.stdout, "systemd:") {
		t.Errorf("annotations without systemd:\n%s", res.stdout)
	}
}

func TestContextLineWhenThereIsNoUnit(t *testing.T) {
	tests := []struct {
		fixture string
		want    string
	}{
		{"non_systemd", "unit: none (not in a systemd unit)"},
		{"synthetic_user_manager", "unit: none (user manager unit, not read)"},
		{"synthetic_pid1_not_systemd", "unit: none (PID 1 is not systemd)"},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			r := &replayRunner{err: errors.New("must not be asked")}
			res := runWithRunner(r, "show", "4242", "--root", systemdRoot(t, tc.fixture))
			if res.code != 0 {
				t.Fatalf("exit %d: %s", res.code, res.stderr)
			}
			if !strings.Contains(res.stdout, "\n"+tc.want+"\n") {
				t.Errorf("stdout lacks %q:\n%s", tc.want, res.stdout)
			}
			// No unit is not a gap: no systemd row appears and nothing is asked.
			if strings.Contains(res.stdout, "systemd.source_error") || len(r.asked) != 0 {
				t.Errorf("asked %v:\n%s", r.asked, res.stdout)
			}
		})
	}
}

func TestShowUnit(t *testing.T) {
	root := unitRoot(t, "all_match")
	r := replayOf(t, "all_match")
	res := runWithRunner(r, "show", "--unit", "le-match.service", "--root", root)
	if res.code != 0 {
		t.Fatalf("exit %d: %s\n%s", res.code, res.stderr, res.stdout)
	}
	if !strings.Contains(res.stdout, "\nunit: le-match.service\n") ||
		!strings.Contains(res.stdout, "systemd: TasksMax=7 on le-match.service") {
		t.Errorf("stdout:\n%s", res.stdout)
	}
	// One question to find the main process, one to annotate it.
	if len(r.asked) != 2 || r.asked[0] != "le-match.service" || r.asked[1] != "le-match.service" {
		t.Errorf("asked %v", r.asked)
	}
	// --json works the same way and stays a bare array.
	js := runWithRunner(replayOf(t, "all_match"), "show", "--unit", "le-match.service", "--root", root, "--json")
	if js.code != 0 || len(decodeJSON(t, js.stdout)) == 0 {
		t.Errorf("--json: exit %d\n%s", js.code, js.stdout)
	}
}

func TestShowUnitFailsWithExitThree(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		runner  func(t *testing.T) systemd.Runner
		unit    string
		want    string
	}{
		{"not found", "not_found", func(t *testing.T) systemd.Runner { return replayOf(t, "not_found") },
			"le-missing.service", "unit le-missing.service not found"},
		{"not found, bare name", "not_found", func(t *testing.T) systemd.Runner { return replayOf(t, "not_found") },
			"le-missing", "unit le-missing not found"},
		{"scope has no main process", "scope", func(t *testing.T) systemd.Runner { return replayOf(t, "scope") },
			"le-scope.scope", "unit le-scope.scope has no main process"},
		{"main pid 0", "all_match", func(*testing.T) systemd.Runner {
			return &replayRunner{out: "LoadState=loaded\nMainPID=0\n"}
		}, "a.service", "unit a.service has no main process"},
		{"no systemd", "synthetic_pid1_not_systemd", func(*testing.T) systemd.Runner { return &replayRunner{} },
			"a.service", "systemd is not available: PID 1 is not systemd"},
		{"not consulted under another root", "all_match", func(*testing.T) systemd.Runner { return nil },
			"a.service", "systemd is not consulted under --root"},
		{"systemctl fails", "all_match", func(*testing.T) systemd.Runner {
			return &replayRunner{err: errors.New("boom")}
		}, "a.service", "failed: boom"},
		{"garbage", "all_match", func(*testing.T) systemd.Runner {
			return &replayRunner{out: "garbage\n"}
		}, "a.service", "unparseable output"},
		{"main process is gone", "all_match", func(*testing.T) systemd.Runner {
			return &replayRunner{out: "LoadState=loaded\nMainPID=987654\n"}
		}, "a.service", "no such process: 987654"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runWithRunner(tc.runner(t), "show", "--unit", tc.unit, "--root", systemdRoot(t, tc.fixture))
			if res.code != 3 {
				t.Errorf("exit %d, want 3\n%s", res.code, res.stderr)
			}
			if !strings.Contains(res.stderr, tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", res.stderr, tc.want)
			}
			if res.stdout != "" {
				t.Errorf("stdout = %q, want empty", res.stdout)
			}
		})
	}
}

func TestShowUnitUsageErrors(t *testing.T) {
	root := systemdRoot(t, "all_match")
	tests := []struct {
		name string
		args []string
	}{
		{"both", []string{"show", "4242", "--unit", "a.service", "--root", root}},
		{"neither", []string{"show", "--root", root}},
		{"empty unit", []string{"show", "--unit", "", "--root", root}},
		{"leading dash", []string{"show", "--unit=-x", "--root", root}},
		{"option lookalike", []string{"show", "--unit=--help", "--root", root}},
		{"space", []string{"show", "--unit", "foo bar", "--root", root}},
		{"slash", []string{"show", "--unit", "a/b.service", "--root", root}},
		{"too long", []string{"show", "--unit", strings.Repeat("a", 256), "--root", root}},
		{"non-ASCII", []string{"show", "--unit", "caf\xc3\xa9.service", "--root", root}},
		{"two pids", []string{"show", "1", "2", "--root", root}},
		{"unit without a value", []string{"show", "--unit"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &replayRunner{err: errors.New("must not be asked")}
			res := runWithRunner(r, tc.args...)
			if res.code != 2 {
				t.Errorf("exit %d, want 2\n%s", res.code, res.stderr)
			}
			if len(r.asked) != 0 {
				t.Errorf("systemctl was asked about %v for an invalid request", r.asked)
			}
			if res.stdout != "" || !strings.Contains(res.stderr, "Try 'limits-explain --help'") {
				t.Errorf("stdout %q, stderr %q", res.stdout, res.stderr)
			}
		})
	}
}

var gapCount = regexp.MustCompile(`\((\d+) limits? not measured\)`)

func gaps(t *testing.T, stdout string) int {
	t.Helper()
	m := gapCount.FindStringSubmatch(lines(stdout)[1])
	if m == nil {
		t.Fatalf("no gap count in the headline:\n%s", stdout)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSystemdFailureIsAVisibleGap covers a systemctl that fails or prints
// garbage: the report is still produced (exit 0) with one systemd.source_error
// row, which counts as a gap exactly like any other source's failure.
func TestSystemdFailureIsAVisibleGap(t *testing.T) {
	root := systemdRoot(t, "all_match")
	baseline := gaps(t, runWithRunner(replayOf(t, "all_match"), "show", "4242", "--root", root).stdout)

	tests := []struct {
		name   string
		runner *replayRunner
		status string
		want   string
	}{
		{"systemctl fails", &replayRunner{err: errors.New("boom")}, "unreadable", "failed: boom"},
		{"garbage", &replayRunner{out: "not key value\n"}, "unparseable", "unparseable output"},
		{"duplicate key", replayOf(t, "synthetic_duplicate_key"), "unparseable", `repeats the key "TasksMax"`},
		{"not loaded", replayOf(t, "not_found"), "unparseable", "LoadState"},
		{"control group elsewhere", replayOf(t, "synthetic_controlgroup_elsewhere"), "unparseable",
			"neither the process cgroup"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runWithRunner(tc.runner, "show", "4242", "--root", root)
			if res.code != 0 {
				t.Fatalf("exit %d, want 0: %s", res.code, res.stderr)
			}
			if !strings.Contains(res.stdout, "\nunit: le-match.service (directives not read)\n") {
				t.Errorf("no context line:\n%s", res.stdout)
			}
			if got := gaps(t, res.stdout); got != baseline+1 {
				t.Errorf("gap count %d, want %d (baseline plus the systemd row)", got, baseline+1)
			}
			if strings.Contains(res.stdout, "systemd: ") && !strings.Contains(res.stdout, "systemd.source_error") {
				t.Errorf("annotations appeared on a failed query:\n%s", res.stdout)
			}

			js := runWithRunner(tc.runner, "show", "4242", "--json", "--root", root)
			var found map[string]any
			for _, row := range decodeJSON(t, js.stdout) {
				if row["key"] == "systemd.source_error" {
					found = row
				}
			}
			if found == nil {
				t.Fatalf("no systemd.source_error row in:\n%s", js.stdout)
			}
			if found["status"] != tc.status || found["source"] != "systemd" || found["limit"] != nil ||
				!strings.Contains(found["notes"].(string), tc.want) {
				t.Errorf("row = %v, want status %s and notes containing %q", found, tc.status, tc.want)
			}
		})
	}
}

func TestSystemdFailureDoesNotChangeTheExitCode(t *testing.T) {
	// A cgroup file at 100% trips exit 1 whether or not systemd works.
	root := combinedRoot(t, "usage_over_limit")
	res := runWithRunner(&replayRunner{err: errors.New("boom")}, "show", "4242", "--root", root)
	plain := runWithRunner(nil, "show", "4242", "--root", root)
	if res.code != plain.code {
		t.Errorf("exit %d with a failing systemd, %d without", res.code, plain.code)
	}
}

func TestEveryFixtureRendersThroughTheCLI(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "sources", "systemd", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != "not_found" && e.Name() != "scope" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			var r systemd.Runner
			if _, err := os.Stat(filepath.Join(systemdRoot(t, name), "show.txt")); err == nil {
				r = replayOf(t, name)
			}
			res := runWithRunner(r, "show", "4242", "--root", systemdRoot(t, name))
			if res.code != 0 && res.code != 1 {
				t.Errorf("exit %d: %s", res.code, res.stderr)
			}
			if !strings.Contains(res.stdout, "\nunit: ") {
				t.Errorf("no unit line:\n%s", res.stdout)
			}
		})
	}
}

func TestHelpDocumentsUnit(t *testing.T) {
	res := run("show", "--help")
	for _, want := range []string{"--unit NAME", "show <pid> | --unit NAME", `"unit:" line`, "systemd.source_error",
		"both or neither of", "the unit is not found"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("help lacks %q:\n%s", want, res.stdout)
		}
	}
}
