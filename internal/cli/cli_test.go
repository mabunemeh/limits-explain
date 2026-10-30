package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mabunemeh/limits-explain/internal/model"
	"github.com/mabunemeh/limits-explain/internal/sources"
	"github.com/mabunemeh/limits-explain/internal/sources/systemd"
)

// fixtureRoot returns the --root value for a committed rlimits fixture tree.
func fixtureRoot(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join("..", "sources", "rlimits", "testdata", name)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("no such fixture tree: %s (%v)", root, err)
	}
	return root
}

type result struct {
	code   int
	stdout string
	stderr string
}

func run(args ...string) result {
	var stdout, stderr bytes.Buffer
	code := Execute(args, &stdout, &stderr)
	return result{code, stdout.String(), stderr.String()}
}

func TestShowTable(t *testing.T) {
	res := run("show", "4242", "--root", fixtureRoot(t, "basic"))
	if res.code != 0 {
		t.Fatalf("exit %d, stderr: %s", res.code, res.stderr)
	}
	for _, want := range []string{"limits-explain\n", "open files (soft)", "10240", "unlimited"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	if res.stderr != "" {
		t.Errorf("stderr = %q, want empty on success", res.stderr)
	}
}

// decodeJSON unmarshals stdout in full, failing if anything but one JSON
// document is present. Numbers stay as json.Number so 64-bit values are not
// rounded through float64.
func decodeJSON(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.UseNumber()
	var rows []map[string]any
	if err := dec.Decode(&rows); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout has content after the JSON document: %q", stdout)
	}
	if !strings.HasSuffix(stdout, "]\n") {
		t.Errorf("stdout does not end with a newline: %q", stdout)
	}
	return rows
}

func TestShowJSONRoundTrips(t *testing.T) {
	res := run("show", "4242", "--root", fixtureRoot(t, "basic"), "--json")
	if res.code != 0 {
		t.Fatalf("exit %d, stderr: %s", res.code, res.stderr)
	}
	rows := decodeJSON(t, res.stdout)

	keys, names := map[string]bool{}, map[string]bool{}
	for _, row := range rows {
		keys[row["key"].(string)] = true
		names[row["name"].(string)] = true
	}
	for _, k := range []string{"rlimit.nofile.soft", "rlimit.nofile.hard"} {
		if !keys[k] {
			t.Errorf("missing key %q", k)
		}
	}
	for _, n := range []string{"open files (soft)", "open files (hard)"} {
		if !names[n] {
			t.Errorf("missing name %q", n)
		}
	}
}

func TestShowJSONKeepsFull64BitValues(t *testing.T) {
	res := run("show", "4242", "--root", fixtureRoot(t, "drift"), "--json")
	if res.code != 0 {
		t.Fatalf("exit %d, stderr: %s", res.code, res.stderr)
	}
	var found bool
	for _, row := range decodeJSON(t, res.stdout) {
		if row["key"] == "rlimit.fsize.single" {
			found = true
			// RLIM_INFINITY - 1: overflows int64 and float64 precision.
			if got := row["limit"].(json.Number).String(); got != "18446744073709551614" {
				t.Errorf("limit = %s", got)
			}
			if row["usage"] != nil {
				t.Errorf("usage = %v, want null", row["usage"])
			}
		}
	}
	if !found {
		t.Error("rlimit.fsize.single not in output")
	}
}

func TestShowMissingProcessExitsThree(t *testing.T) {
	res := run("show", "9999", "--root", fixtureRoot(t, "basic"))
	if res.code != 3 {
		t.Errorf("exit %d, want 3", res.code)
	}
	if !strings.Contains(res.stderr, "9999") {
		t.Errorf("stderr = %q, want it to name the pid", res.stderr)
	}
	// Machine consumers of stdout never see output on a failure path.
	if res.stdout != "" {
		t.Errorf("stdout = %q, want empty", res.stdout)
	}
}

func TestShowNonexistentRootExitsThree(t *testing.T) {
	res := run("show", "4242", "--root", filepath.Join(t.TempDir(), "absent"))
	if res.code != 3 || res.stdout != "" {
		t.Errorf("exit %d, stdout %q, want 3 and empty", res.code, res.stdout)
	}
}

func TestShowStructuralFailureExitsThree(t *testing.T) {
	for _, name := range []string{"missing_header", "empty_file", "header_only"} {
		t.Run(name, func(t *testing.T) {
			res := run("show", "4242", "--root", fixtureRoot(t, name))
			if res.code != 3 {
				t.Errorf("exit %d, want 3", res.code)
			}
			if res.stdout != "" {
				t.Errorf("stdout = %q, want empty", res.stdout)
			}
			if !strings.Contains(res.stderr, "proc/4242/limits") {
				t.Errorf("stderr = %q, want it to name the file", res.stderr)
			}
		})
	}
}

func TestShowMalformedRowStillExitsZero(t *testing.T) {
	// A single bad row degrades to an unparseable reading instead of
	// aborting the report.
	res := run("show", "4242", "--root", fixtureRoot(t, "malformed"), "--json")
	if res.code != 0 {
		t.Fatalf("exit %d, stderr: %s", res.code, res.stderr)
	}
	var sawBad, sawGood bool
	for _, row := range decodeJSON(t, res.stdout) {
		switch {
		case row["status"] == "unparseable":
			sawBad = true
			if row["limit"] != nil {
				t.Errorf("unparseable row has limit %v, want null", row["limit"])
			}
			if row["name"] != "(unparseable row)" {
				t.Errorf("name = %v", row["name"])
			}
		case row["key"] == "rlimit.nofile.soft":
			sawGood = true
		}
	}
	if !sawBad || !sawGood {
		t.Errorf("sawBad=%v sawGood=%v", sawBad, sawGood)
	}

	// The table must not call the unparseable row unlimited.
	table := run("show", "4242", "--root", fixtureRoot(t, "malformed"))
	for _, line := range strings.Split(table.stdout, "\n") {
		if strings.Contains(line, "(unparseable row)") && strings.Contains(line, "unlimited") {
			t.Errorf("unparseable row claims unlimited: %q", line)
		}
	}
}

func TestShowMarkupInjectionPrintsLiterally(t *testing.T) {
	res := run("show", "4242", "--root", fixtureRoot(t, "markup_injection"))
	if res.code != 0 {
		t.Fatalf("exit %d, stdout %s stderr %s", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "weird [/red]evil[bold thing (soft)") {
		t.Errorf("brackets were not printed literally:\n%s", res.stdout)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	root := fixtureRoot(t, "basic")
	tests := []struct {
		name string
		args []string
	}{
		{"missing pid", []string{"show"}},
		{"non-integer pid", []string{"show", "abc"}},
		{"fractional pid", []string{"show", "1.5"}},
		{"overflowing pid", []string{"show", "99999999999999999999999"}},
		{"zero pid", []string{"show", "0", "--root", root}},
		{"negative pid", []string{"show", "--root", root, "--", "-1"}},
		{"negative pid parsed as flag", []string{"show", "-1", "--root", root}},
		{"pid with a letter", []string{"show", "x4242", "--root", root}},
		{"plus-signed pid", []string{"show", "+4242", "--root", root}},
		{"extra argument", []string{"show", "1", "2", "--root", root}},
		{"unknown flag", []string{"show", "4242", "--bogus", "--root", root}},
		{"unknown command", []string{"bogus"}},
		{"bare pid without show", []string{"4242"}},
		{"empty root", []string{"show", "4242", "--root", ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := run(tc.args...)
			if res.code != 2 {
				t.Errorf("exit %d, want 2 (stderr: %s)", res.code, res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("stdout = %q, want empty", res.stdout)
			}
			if !strings.HasPrefix(res.stderr, "limits-explain: ") {
				t.Errorf("stderr = %q", res.stderr)
			}
		})
	}
}

func TestEmptyRootIsNotTheCurrentDirectory(t *testing.T) {
	res := run("show", "4242", "--root", "")
	if res.code != 2 || !strings.Contains(res.stderr, "--root must not be empty") {
		t.Errorf("exit %d, stderr %q", res.code, res.stderr)
	}
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"-h"}, {"--help"}, {"help"}} {
		res := run(args...)
		if res.code != 0 {
			t.Errorf("run(%q): exit %d, stderr %q", args, res.code, res.stderr)
		}
		if !strings.Contains(res.stdout, "show") {
			t.Errorf("run(%q): help does not mention show:\n%s", args, res.stdout)
		}
	}

	res := run("show", "--help")
	if res.code != 0 {
		t.Fatalf("exit %d", res.code)
	}
	if !strings.Contains(res.stdout, "--json") {
		t.Errorf("show help lacks --json:\n%s", res.stdout)
	}
	if strings.Contains(res.stdout, "--root") {
		t.Errorf("show help advertises the hidden --root flag:\n%s", res.stdout)
	}
	if h := run("show", "-h"); h.code != 0 || h.stdout != res.stdout {
		t.Errorf("-h differs from --help (exit %d)", h.code)
	}
}

// failingFS delegates to a real tree but fails every open of a limits file,
// modelling a process that exits (or a permission change) after the
// existence check passed but before the read.
type failingFS struct {
	fs.FS
	err error
}

func (f failingFS) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, "/limits") {
		return nil, &fs.PathError{Op: "open", Path: name, Err: f.err}
	}
	return f.FS.Open(name)
}

func TestProcessDyingBetweenStatAndReadExitsThree(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"gone", fs.ErrNotExist, "unreadable"},
		{"permission", fs.ErrPermission, "permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			open := func(root string) fs.FS { return failingFS{os.DirFS(root), tc.err} }
			code := execute([]string{"show", "4242", "--root", fixtureRoot(t, "basic")}, &stdout, &stderr, open,
				func(string) systemd.Runner { return nil })
			if code != 3 {
				t.Errorf("exit %d, want 3", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
		})
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestOutputWriteFailureExitsFour(t *testing.T) {
	var stderr bytes.Buffer
	code := Execute([]string{"show", "4242", "--root", fixtureRoot(t, "basic")}, brokenWriter{}, &stderr)
	if code != 4 {
		t.Errorf("exit %d, want 4", code)
	}
	if !strings.Contains(stderr.String(), "broken pipe") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// cgroupRoot returns the --root value for a committed cgroup fixture tree.
func cgroupRoot(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join("..", "sources", "cgroup", "testdata", name)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("no such fixture tree: %s (%v)", root, err)
	}
	return root
}

// combinedRoot copies a cgroup fixture into a temp dir and adds the rlimits
// "basic" limits file, giving a root where both sources can succeed (or,
// with a v1 tree, where cgroup fails and rlimits still succeeds). Copying
// keeps every fixture committed exactly once.
func combinedRoot(t *testing.T, cgroupFixture string) string {
	t.Helper()
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(cgroupRoot(t, cgroupFixture))); err != nil {
		t.Fatal(err)
	}
	limits, err := os.ReadFile(filepath.Join(fixtureRoot(t, "basic"), "proc", "4242", "limits"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "proc", "4242", "limits"), limits, 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

// rowsByKey decodes --json output into a map keyed by reading key.
func rowsByKey(t *testing.T, stdout string) map[string]map[string]any {
	t.Helper()
	m := map[string]map[string]any{}
	for _, row := range decodeJSON(t, stdout) {
		m[row["key"].(string)] = row
	}
	return m
}

func TestPartialFailureRlimitsUpCgroupDown(t *testing.T) {
	// The rlimits fixtures have no proc/4242/cgroup, so the cgroup source
	// fails while rlimits succeeds: exit 0, both visible.
	root := fixtureRoot(t, "basic")
	table := run("show", "4242", "--root", root)
	if table.code != 0 || table.stderr != "" {
		t.Fatalf("exit %d, stderr %q", table.code, table.stderr)
	}
	for _, want := range []string{"open files (soft)", "cgroup source", "cgroup", "unreadable", "proc/4242/cgroup"} {
		if !strings.Contains(table.stdout, want) {
			t.Errorf("table lacks %q:\n%s", want, table.stdout)
		}
	}

	res := run("show", "4242", "--root", root, "--json")
	if res.code != 0 || res.stderr != "" {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	rows := rowsByKey(t, res.stdout)
	if _, ok := rows["rlimit.nofile.soft"]; !ok {
		t.Error("rlimit rows are missing")
	}
	row, ok := rows["cgroup.source_error"]
	if !ok {
		t.Fatalf("no cgroup.source_error row: %s", res.stdout)
	}
	if row["status"] != "unreadable" || row["source"] != "cgroup" || row["label"] != "cgroup source" ||
		row["kind"] != "single" || row["limit"] != nil || row["usage"] != nil || row["unit"] != "" {
		t.Errorf("degraded row = %v", row)
	}
	if notes, _ := row["notes"].(string); !strings.Contains(notes, "proc/4242/cgroup: unreadable") {
		t.Errorf("notes = %q, want the full error text", notes)
	}
}

func TestPartialFailureCgroupUpRlimitsDown(t *testing.T) {
	root := cgroupRoot(t, "nested_ancestor_binds")
	res := run("show", "4242", "--root", root, "--json")
	if res.code != 0 || res.stderr != "" {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	rows := rowsByKey(t, res.stdout)
	if r := rows["cgroup.pids.max"]; r == nil || r["limit"].(json.Number).String() != "50" {
		t.Errorf("cgroup.pids.max = %v", r)
	}
	row, ok := rows["rlimit.source_error"]
	if !ok {
		t.Fatalf("no rlimit.source_error row: %s", res.stdout)
	}
	if row["status"] != "unreadable" || row["source"] != "rlimit" || row["label"] != "rlimit source" {
		t.Errorf("degraded row = %v", row)
	}
	if table := run("show", "4242", "--root", root); table.code != 0 ||
		!strings.Contains(table.stdout, "rlimit source") || !strings.Contains(table.stdout, "tasks") {
		t.Errorf("table: exit %d\n%s", table.code, table.stdout)
	}
}

func TestBothSourcesSucceed(t *testing.T) {
	root := combinedRoot(t, "nested_ancestor_binds")
	res := run("show", "4242", "--root", root, "--json")
	if res.code != 0 || res.stderr != "" {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	rows := rowsByKey(t, res.stdout)
	for _, k := range []string{"rlimit.nofile.soft", "cgroup.memory.max", "cgroup.pids.max"} {
		if rows[k] == nil {
			t.Errorf("missing %q", k)
		}
	}
	for k := range rows {
		if strings.HasSuffix(k, ".source_error") {
			t.Errorf("unexpected degraded row %q", k)
		}
	}
}

func TestV1HostKeepsRlimitsAndShowsTheV1Message(t *testing.T) {
	root := combinedRoot(t, "v1")
	res := run("show", "4242", "--root", root, "--json")
	if res.code != 0 || res.stderr != "" {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	rows := rowsByKey(t, res.stdout)
	if rows["rlimit.nofile.soft"] == nil {
		t.Error("the rlimit rows were lost to the cgroup failure")
	}
	row := rows["cgroup.source_error"]
	if row == nil {
		t.Fatalf("no cgroup.source_error row: %s", res.stdout)
	}
	// The input was read but has no usable v2 structure: unparseable.
	if row["status"] != "unparseable" {
		t.Errorf("status = %v, want unparseable", row["status"])
	}
	if notes, _ := row["notes"].(string); !strings.Contains(notes, "cgroup v1 hierarchy") {
		t.Errorf("notes = %q, want the v1 message", notes)
	}
	if table := run("show", "4242", "--root", root); !strings.Contains(table.stdout, "cgroup v1 hierarchy") {
		t.Errorf("table lacks the v1 message:\n%s", table.stdout)
	}
}

func TestAllSourcesFailingExitsThreeWithEveryError(t *testing.T) {
	res := run("show", "4242", "--root", cgroupRoot(t, "v1"))
	if res.code != 3 || res.stdout != "" {
		t.Errorf("exit %d, stdout %q, want 3 and empty", res.code, res.stdout)
	}
	// errors.Join: both sources' errors are reported, not just the first.
	for _, want := range []string{"cgroup v1 hierarchy", "proc/4242/limits"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, res.stderr)
		}
	}
}

func TestSourceFailureStatusFollowsTheErrorKind(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want model.Status
	}{
		{"read error", &sources.SourceError{Path: "p", Detail: "d", Err: fs.ErrPermission}, model.StatusUnreadable},
		{"structure error", &sources.SourceError{Path: "p", Detail: "d"}, model.StatusUnparseable},
		{"wrapped structure error", fmt.Errorf("ctx: %w", &sources.SourceError{Path: "p", Detail: "d"}), model.StatusUnparseable},
		{"not a SourceError", errors.New("boom"), model.StatusUnreadable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := sourceFailure("x", tc.err)
			if r.Status != tc.want || r.Key != "x.source_error" || r.Notes != tc.err.Error() {
				t.Errorf("reading = %+v", r)
			}
		})
	}
}

func TestVersion(t *testing.T) {
	saved := version
	t.Cleanup(func() { version = saved })

	version = "v9.8.7"
	res := run("--version")
	if res.code != 0 || res.stdout != "limits-explain v9.8.7\n" || res.stderr != "" {
		t.Errorf("stamped: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}

	// A test binary records no module version, so an unstamped build says dev.
	version = ""
	if got := run("--version").stdout; got != "limits-explain dev\n" {
		t.Errorf("unstamped: stdout %q, want %q", got, "limits-explain dev\n")
	}
}
