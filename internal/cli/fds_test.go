package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fdsRoot builds a root from a committed fds fixture (fd directory and
// status) plus the rlimits "basic" limits file with its open-files row
// replaced by the given soft and hard values.
func fdsRoot(t *testing.T, fixture, soft, hard string) string {
	t.Helper()
	dst := t.TempDir()
	src := filepath.Join("..", "sources", "fds", "testdata", fixture)
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	limits, err := os.ReadFile(filepath.Join(fixtureRoot(t, "basic"), "proc", "4242", "limits"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(limits)
	const old = "10240                1048576"
	if !strings.Contains(text, old) {
		t.Fatal("basic limits fixture no longer has the expected open files row")
	}
	text = strings.Replace(text, old, soft+" "+hard, 1)
	if err := os.WriteFile(filepath.Join(dst, "proc", "4242", "limits"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestNofileUsageTripsWarnAndRanksFirst(t *testing.T) {
	// Eight descriptors against a soft limit of 8 is 100%.
	root := fdsRoot(t, "at_soft", "8", "16")
	res := run("show", "4242", "--root", root, "--json")
	if res.code != 1 {
		t.Fatalf("exit %d, want 1 (stderr: %s)", res.code, res.stderr)
	}
	rows := decodeJSON(t, res.stdout)
	if got := rows[0]["key"]; got != "rlimit.nofile.soft" {
		t.Fatalf("first row = %v, want rlimit.nofile.soft", got)
	}
	if got := rows[0]["status"]; got != "ok" {
		t.Errorf("status = %v, want ok", got)
	}
	if got := fmt.Sprint(rows[0]["usage"]); got != "8" {
		t.Errorf("usage = %v, want 8", got)
	}
	// The hard row is measured too: 8 of 16 is 50%, so it is not the
	// reading that tripped --warn, but it is no longer a gap.
	for _, r := range rows {
		if r["key"] == "rlimit.nofile.hard" && r["status"] != "ok" {
			t.Errorf("hard row status = %v, want ok", r["status"])
		}
	}
}

func TestNofileUsageBelowWarnExitsZero(t *testing.T) {
	root := fdsRoot(t, "normal", "100", "200")
	res := run("show", "4242", "--root", root)
	if res.code != 0 {
		t.Fatalf("exit %d, want 0 (stderr: %s)", res.code, res.stderr)
	}
}

func TestMissingFdDirectoryIsANoteNotARow(t *testing.T) {
	res := run("show", "4242", "--root", fixtureRoot(t, "basic"), "--json")
	if res.code != 0 {
		t.Fatalf("exit %d, want 0 (stderr: %s)", res.code, res.stderr)
	}
	for _, r := range decodeJSON(t, res.stdout) {
		// The cgroup source has no tree here and fails on its own; the fd
		// pass must not add a row of its own.
		if r["source"] == "fds" || r["key"] == "fds.source_error" {
			t.Errorf("unexpected fds row: %v", r)
		}
		if r["key"] == "rlimit.nofile.soft" {
			if r["status"] != "unmeasured" || !strings.Contains(r["notes"].(string), "fd count unreadable") {
				t.Errorf("nofile soft = %v", r)
			}
		}
	}
}
