//go:build integration && linux

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These cases each start their own transient unit on the host, run the
// in-process CLI against the live /proc and /sys/fs/cgroup, and assert on row
// keys, statuses and numbers, never on display labels or column layout. Unit
// names are unique so a leftover from one case cannot satisfy another.

// row is one reading of the --json envelope.
type row struct {
	Key    string  `json:"key"`
	Status string  `json:"status"`
	Limit  *uint64 `json:"limit"`
	Usage  *uint64 `json:"usage"`
	Notes  string  `json:"notes"`
}

type envelope struct {
	Readings []row `json:"readings"`
	Summary  struct {
		Warned int `json:"warned"`
	} `json:"summary"`
	Unit struct {
		DirectivesRead bool    `json:"directives_read"`
		Name           *string `json:"name"`
	} `json:"unit"`
}

// run executes the CLI and decodes its --json output. Exit codes other than
// the ones in allowed fail the test.
func run(t *testing.T, allowed []int, args ...string) (int, envelope) {
	t.Helper()
	code, stdout, stderr := show(args...)
	ok := false
	for _, a := range allowed {
		ok = ok || a == code
	}
	if !ok {
		t.Fatalf("exit %d (allowed %v), stderr: %s\n%s", code, allowed, stderr, stdout)
	}
	var env envelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not the JSON envelope: %v\n%s", err, stdout)
	}
	for _, r := range env.Readings {
		if r.Key == "systemd.source_error" {
			t.Errorf("systemd failed: %s", r.Notes)
		}
	}
	return code, env
}

// find returns the row with key.
func (e envelope) find(t *testing.T, key string) row {
	t.Helper()
	for _, r := range e.Readings {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("no %s row among %d readings", key, len(e.Readings))
	return row{}
}

func requireSystemd(t *testing.T) {
	t.Helper()
	if !isSystemdPID1() {
		t.Skip("requires a real systemd PID 1")
	}
}

func wantNote(t *testing.T, r row, want string) {
	t.Helper()
	if !strings.Contains(r.Notes, want) {
		t.Errorf("%s notes = %q, want them to contain %q", r.Key, r.Notes, want)
	}
}

func wantAmount(t *testing.T, r row, what string, got *uint64, want uint64) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s %s = %v, want %d", r.Key, what, got, want)
	}
}

// pidsCurrent reads pids.current of a unit's own cgroup.
func pidsCurrent(t *testing.T, unit string) (uint64, bool) {
	t.Helper()
	out, err := output("systemctl", "show", "-p", "ControlGroup", "--value", unit+".service")
	if err != nil {
		t.Fatalf("systemctl show: %v", err)
	}
	b, err := os.ReadFile("/sys/fs/cgroup" + strings.TrimSpace(out) + "/pids.current")
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return n, err == nil
}

// startThreeTasks starts a unit whose main process is a shell with two
// sleeping children: exactly three tasks. It returns once pids.current says so.
func startThreeTasks(t *testing.T, name string, flags []string, props ...string) {
	t.Helper()
	startUnitWith(t, name, flags, []string{"sh", "-c", "sleep 60 & sleep 60 & wait"}, props...)
	waitFor(t, name+" to hold 3 tasks", func() bool {
		n, ok := pidsCurrent(t, name)
		return ok && n == 3
	})
}

func TestCLITasksMaxReached(t *testing.T) {
	requireSystemd(t)
	const name = "limits-explain-tasksmax"
	startThreeTasks(t, name, nil, "TasksMax=3")

	for _, args := range [][]string{
		{"show", "--unit", name, "--json"},
		{"show", "--unit", name, "--json", "--warn", "100"},
	} {
		code, env := run(t, []int{1}, args...)
		if code != 1 {
			t.Fatalf("%v: exit %d, want 1", args, code)
		}
		r := env.find(t, "cgroup.pids.max")
		if r.Status != "ok" {
			t.Errorf("%v: pids status %q, want ok", args, r.Status)
		}
		wantAmount(t, r, "limit", r.Limit, 3)
		wantAmount(t, r, "usage", r.Usage, 3)
		wantNote(t, r, "systemd: TasksMax=3 on "+name+".service")
		if env.Summary.Warned < 1 {
			t.Errorf("%v: summary.warned = %d, want at least 1", args, env.Summary.Warned)
		}
	}
}

func TestCLIMemoryMaxAndHigh(t *testing.T) {
	requireSystemd(t)
	const name = "limits-explain-memory"
	startUnit(t, name, "MemoryMax=64M", "MemoryHigh=32M")

	_, env := run(t, []int{0, 1}, "show", "--unit", name, "--json")
	for _, c := range []struct {
		key   string
		limit uint64
		note  string
	}{
		{"cgroup.memory.max", 64 << 20, "systemd: MemoryMax=67108864 on " + name + ".service"},
		{"cgroup.memory.high", 32 << 20, "systemd: MemoryHigh=33554432 on " + name + ".service"},
	} {
		r := env.find(t, c.key)
		if r.Status != "ok" {
			t.Errorf("%s status %q, want ok", c.key, r.Status)
		}
		wantAmount(t, r, "limit", r.Limit, c.limit)
		if r.Usage == nil || *r.Usage == 0 {
			t.Errorf("%s usage = %v, want measured and > 0", c.key, r.Usage)
		}
		wantNote(t, r, c.note)
	}
}

func TestCLICPUQuota(t *testing.T) {
	requireSystemd(t)
	const name = "limits-explain-cpu"
	startUnit(t, name, "CPUQuota=50%")

	_, env := run(t, []int{0, 1}, "show", "--unit", name, "--json")
	r := env.find(t, "cgroup.cpu.max")
	// cpu.max "50000 100000" is derived to 500 millicores. There is no CPU
	// usage counter, so the row stays unmeasured.
	if r.Status != "unmeasured" || r.Usage != nil {
		t.Errorf("cpu.max status %q usage %v, want unmeasured and no usage", r.Status, r.Usage)
	}
	wantAmount(t, r, "limit", r.Limit, 500)
	wantNote(t, r, `raw cpu.max "50000 100000"`)
	wantNote(t, r, "systemd: CPUQuotaPerSecUSec=500ms")
	wantNote(t, r, " on "+name+".service")
}

func TestCLILimitNPROCStaysUnmeasured(t *testing.T) {
	requireSystemd(t)
	const name = "limits-explain-nproc"
	startUnit(t, name, "LimitNPROC=4096")

	_, env := run(t, []int{0, 1}, "show", "--unit", name, "--json")
	seen := 0
	for _, r := range env.Readings {
		if !strings.HasPrefix(r.Key, "rlimit.nproc.") {
			continue
		}
		seen++
		if r.Status != "unmeasured" || r.Usage != nil {
			t.Errorf("%s status %q usage %v, want unmeasured and no usage", r.Key, r.Status, r.Usage)
		}
		wantAmount(t, r, "limit", r.Limit, 4096)
		wantNote(t, r, "per-user limit")
		wantNote(t, r, "systemd: LimitNPROC=4096")
	}
	if seen == 0 {
		t.Error("no rlimit.nproc.* row in the report")
	}
}

// TestCLILimitNOFILE accepts the single row (soft equals hard, as the unit sets
// here) or a soft/hard pair: which one appears depends on how the rlimits
// source merges equal values, and GitHub's runners have equal soft and hard
// limits by default.
func TestCLILimitNOFILE(t *testing.T) {
	requireSystemd(t)
	const name = "limits-explain-nofile"
	startUnit(t, name, "LimitNOFILE=2048")

	_, env := run(t, []int{0, 1}, "show", "--unit", name, "--json")
	seen := 0
	for _, r := range env.Readings {
		switch r.Key {
		case "rlimit.nofile.single", "rlimit.nofile.soft", "rlimit.nofile.hard":
		default:
			continue
		}
		seen++
		wantAmount(t, r, "limit", r.Limit, 2048)
		wantNote(t, r, "systemd: LimitNOFILE")
	}
	if seen == 0 {
		t.Error("no rlimit.nofile.* row in the report")
	}
}

func TestCLIAncestorSliceBinds(t *testing.T) {
	requireSystemd(t)
	const (
		slice = "limits-explain-slice.slice"
		name  = "limits-explain-nested"
	)
	if out, err := output("sudo", "systemctl", "start", slice); err != nil {
		t.Fatalf("starting %s: %v\n%s", slice, err, out)
	}
	// A dash in a slice name nests it, so systemd also activates the parents
	// limits.slice and limits-explain.slice. Stop them too, outermost last.
	t.Cleanup(func() {
		for _, s := range []string{slice, "limits-explain.slice", "limits.slice"} {
			stopUnit(t, s)
		}
	})
	if out, err := output("sudo", "systemctl", "set-property", "--runtime", slice, "TasksMax=3"); err != nil {
		t.Fatalf("set-property: %v\n%s", err, out)
	}
	// The unit keeps its own, much looser, TasksMax; the slice is tighter.
	startThreeTasks(t, name, []string{"--slice=" + slice}, "TasksMax=100")

	code, env := run(t, []int{1}, "show", "--unit", name, "--json")
	if code != 1 {
		t.Fatalf("exit %d, want 1 (the slice is full)", code)
	}
	r := env.find(t, "cgroup.pids.max")
	if r.Status != "ok" {
		t.Errorf("pids status %q, want ok", r.Status)
	}
	wantAmount(t, r, "limit", r.Limit, 3)
	wantAmount(t, r, "usage", r.Usage, 3)
	// The cgroup path above the slice depends on the dashes in its name, so
	// match the slice as the last path component.
	nearest := regexp.MustCompile(`nearest its ceiling: (\S*/)?` + regexp.QuoteMeta(slice) + ` \(`)
	if !nearest.MatchString(r.Notes) {
		t.Errorf("pids notes = %q, want the slice named as nearest its ceiling", r.Notes)
	}
	wantNote(t, r, "allows 100")
	wantNote(t, r, "systemd: TasksMax=100 on "+name+".service")
}

func TestCLIScopeHasNoMainPID(t *testing.T) {
	requireSystemd(t)
	const name = "limits-explain-scope"
	// --scope runs the command in the foreground as a child of systemd-run, so
	// it is started in the background and found through the scope's cgroup.
	cmd := exec.Command("sudo", "systemd-run", "--scope", "--quiet", //nolint:gosec // fixed literals
		"--unit="+name, "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting scope: %v", err)
	}
	t.Cleanup(func() {
		stopUnit(t, name+".scope")
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(subprocessTimeout):
			_ = cmd.Process.Kill()
		}
	})

	var pid int
	waitFor(t, "the scope to hold a process", func() bool {
		out, err := output("systemctl", "show", "-p", "ControlGroup", "--value", name+".scope")
		if err != nil || strings.TrimSpace(out) == "" {
			return false
		}
		procs, err := os.ReadFile("/sys/fs/cgroup" + strings.TrimSpace(out) + "/cgroup.procs")
		if err != nil {
			return false
		}
		fields := strings.Fields(string(procs))
		if len(fields) == 0 {
			return false
		}
		comm, err := os.ReadFile("/proc/" + fields[0] + "/comm")
		if err != nil || strings.TrimSpace(string(comm)) != "sleep" {
			return false
		}
		pid, err = strconv.Atoi(fields[0])
		return err == nil
	})

	_, env := run(t, []int{0, 1}, "show", strconv.Itoa(pid), "--json")
	if env.Unit.Name == nil || *env.Unit.Name != name+".scope" || !env.Unit.DirectivesRead {
		t.Errorf("unit = %+v, want %s.scope with directives read", env.Unit, name)
	}
}
