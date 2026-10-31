//go:build integration

// Package integration holds the tests that need a real systemd host.
//
// The systemd spike reads real rlimits back from a systemd-managed process.
// It de-risks the future systemd source by proving, on a real host, that a
// transient unit's PID is resolvable and that its /proc/<pid>/limits is
// readable through the rlimits source with the real root, no fixtures.
//
// It is behind the "integration" build tag AND skips at run time unless PID 1
// is systemd (GitHub Actions ubuntu-latest runners qualify; a developer
// laptop running another init system does not), since it spawns and stops a
// real transient unit through sudo systemd-run. The CLI tests go further and
// run the in-process CLI against the live host with the real systemctl runner.
// Run it with:
//
//	go test -tags=integration -v ./internal/integration/...
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mabunemeh/limits-explain/internal/cli"
	"github.com/mabunemeh/limits-explain/internal/sources/rlimits"
)

const unitName = "limits-explain-spike"

// Every subprocess below talks to systemd on the host. A hang there, rather
// than a clean failure, must not be able to wedge the whole CI job.
const subprocessTimeout = 30 * time.Second

// systemctl show -p MainPID can report 0 for a brief moment right after
// systemd-run returns (the unit is accepted before the manager has recorded
// the forked PID). Poll instead of reading it once, or this spike is flaky.
const (
	mainPIDPollTimeout  = 5 * time.Second
	mainPIDPollInterval = 200 * time.Millisecond
)

func isSystemdPID1() bool {
	comm, err := os.ReadFile("/proc/1/comm")
	return err == nil && strings.TrimSpace(string(comm)) == "systemd"
}

// output runs a command with the subprocess deadline and returns its stdout.
func output(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), subprocessTimeout)
	defer cancel()
	// The command and arguments are fixed literals in this file, not input.
	out, err := exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // see above
	return string(out), err
}

// waitForMainPID polls systemctl until the unit reports a real MainPID.
func waitForMainPID(t *testing.T, unit string) int {
	t.Helper()
	deadline := time.Now().Add(mainPIDPollTimeout)
	var last string
	for time.Now().Before(deadline) {
		out, err := output("systemctl", "show", "-p", "MainPID", "--value", unit+".service")
		if err != nil {
			t.Fatalf("systemctl show: %v", err)
		}
		last = strings.TrimSpace(out)
		if pid, err := strconv.Atoi(last); err == nil && pid > 0 {
			return pid
		}
		time.Sleep(mainPIDPollInterval)
	}
	t.Fatalf("MainPID never became non-zero within %s (last read: %q)", mainPIDPollTimeout, last)
	return 0
}

func TestTransientUnitRlimitsAreReadable(t *testing.T) {
	if !isSystemdPID1() {
		t.Skip("requires a real systemd PID 1")
	}

	if out, err := output("sudo", "systemd-run",
		"--unit="+unitName, "--property=TasksMax=7", "sleep", "30"); err != nil {
		t.Fatalf("systemd-run: %v\n%s", err, out)
	}
	defer func() {
		if out, err := output("sudo", "systemctl", "stop", unitName); err != nil {
			t.Logf("stopping %s: %v\n%s", unitName, err, out)
		}
	}()

	pid := waitForMainPID(t, unitName)

	// The real root: no fixtures, the genuine /proc of the host.
	readings, err := rlimits.New().Collect(pid, os.DirFS("/"))
	if err != nil {
		t.Fatalf("Collect(%d): %v", pid, err)
	}
	if len(readings) == 0 {
		t.Fatal("expected at least one rlimit reading from the transient unit")
	}
	// Key (not a display name) is the stable field to assert on.
	for _, r := range readings {
		if strings.HasPrefix(r.Key, "rlimit.nproc.") {
			return
		}
	}
	t.Errorf("no rlimit.nproc.* key among %d readings", len(readings))
}

// startUnit starts a transient unit with the given properties and returns its
// MainPID. The unit is stopped when the test ends.
func startUnit(t *testing.T, name string, props ...string) int {
	t.Helper()
	args := []string{"systemd-run", "--quiet", "--unit=" + name}
	for _, p := range props {
		args = append(args, "--property="+p)
	}
	args = append(args, "sleep", "30")
	if out, err := output("sudo", args...); err != nil {
		t.Fatalf("systemd-run: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := output("sudo", "systemctl", "stop", name); err != nil {
			t.Logf("stopping %s: %v\n%s", name, err, out)
		}
	})
	return waitForMainPID(t, name)
}

// show runs the in-process CLI against the live host: the default root, so the
// real systemctl runner is used.
func show(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := cli.Execute(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func notesByKey(t *testing.T, stdout string) map[string]string {
	t.Helper()
	var env struct {
		Readings []map[string]any `json:"readings"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not the JSON envelope: %v\n%s", err, stdout)
	}
	notes := map[string]string{}
	for _, row := range env.Readings {
		key, _ := row["key"].(string)
		notes[key], _ = row["notes"].(string)
	}
	return notes
}

func TestCLIAnnotatesATransientUnit(t *testing.T) {
	if !isSystemdPID1() {
		t.Skip("requires a real systemd PID 1")
	}
	const name = "limits-explain-cli"
	pid := startUnit(t, name, "TasksMax=7", "LimitNOFILE=4096:8192")

	want := map[string]string{
		"cgroup.pids.max":    "systemd: TasksMax=7 on " + name + ".service",
		"rlimit.nofile.soft": "systemd: LimitNOFILESoft=4096 on " + name + ".service",
		"rlimit.nofile.hard": "systemd: LimitNOFILE=8192 on " + name + ".service",
	}
	check := func(t *testing.T, args ...string) {
		t.Helper()
		code, stdout, stderr := show(args...)
		if code != 0 {
			t.Fatalf("exit %d, stderr: %s\n%s", code, stderr, stdout)
		}
		notes := notesByKey(t, stdout)
		if note, ok := notes["systemd.source_error"]; ok {
			t.Errorf("systemd failed: %s", note)
		}
		for key, text := range want {
			if !strings.Contains(notes[key], text) {
				t.Errorf("%s notes = %q, want them to contain %q", key, notes[key], text)
			}
		}
	}
	t.Run("by pid", func(t *testing.T) { check(t, "show", strconv.Itoa(pid), "--json") })
	t.Run("by unit", func(t *testing.T) { check(t, "show", "--unit", name+".service", "--json") })
	t.Run("by bare unit name", func(t *testing.T) { check(t, "show", "--unit", name, "--json") })

	t.Run("table names the unit", func(t *testing.T) {
		code, stdout, stderr := show("show", strconv.Itoa(pid))
		if code != 0 || !strings.Contains(stdout, "\nunit: "+name+".service\n") {
			t.Errorf("exit %d, stderr %q\n%s", code, stderr, stdout)
		}
	})
}

func TestCLIUnitNotFound(t *testing.T) {
	if !isSystemdPID1() {
		t.Skip("requires a real systemd PID 1")
	}
	code, stdout, stderr := show("show", "--unit", "limits-explain-no-such-unit.service")
	if code != 3 || stdout != "" || !strings.Contains(stderr, "not found") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}
