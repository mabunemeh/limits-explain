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
// real transient unit through sudo systemd-run. Run it with:
//
//	go test -tags=integration -v ./internal/integration/...
package integration

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

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
