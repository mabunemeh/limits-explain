package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mabunemeh/limits-explain/internal/sources/systemd"
)

// update rewrites the golden files of the host fixtures instead of comparing
// against them:
//
//	go test ./internal/cli -run Hosts -update
var update = flag.Bool("update", false, "rewrite the golden files under testdata/hosts")

const hostsDir = "testdata/hosts"

// windowsNotExist is syscall.ERROR_FILE_NOT_FOUND as Windows words it.
const windowsNotExist = "The system cannot find the file specified."

// failingRunner stands in for systemctl on a capture that has no show.txt. The
// capture says systemd was never asked, so being asked is a test failure.
type failingRunner struct{ t *testing.T }

func (r failingRunner) Show(unit string, _ []string) (string, error) {
	r.t.Errorf("systemctl was asked about %q, but the capture has no show.txt", unit)
	return "", errors.New("unexpected systemctl call")
}

// TestHosts walks every capture under testdata/hosts. Each directory is a tree
// made by scripts/capture-fixture.sh (proc/4242, sys/fs/cgroup, an optional
// show.txt and a host.txt that is documentation only) plus two golden files:
// golden.json, the whole stdout of `show 4242 --json --root <tree>`, and
// golden.exit, the exit code. Adding a host is dropping in a capture and
// running the test with -update.
func TestHosts(t *testing.T) {
	entries, err := os.ReadDir(hostsDir)
	if err != nil {
		t.Fatal(err)
	}
	cases := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cases++
		t.Run(e.Name(), func(t *testing.T) { checkHost(t, filepath.Join(hostsDir, e.Name())) })
	}
	if cases == 0 {
		t.Fatalf("no captures under %s", hostsDir)
	}
}

func checkHost(t *testing.T, tree string) {
	t.Helper()
	var runner systemd.Runner = failingRunner{t}
	if b, err := os.ReadFile(filepath.Join(tree, "show.txt")); err == nil {
		runner = &replayRunner{out: string(b)}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	res := runWithRunner(runner, "show", "4242", "--json", "--root", tree)
	if res.code != 0 && res.code != 1 {
		t.Fatalf("exit %d, stderr: %s\n%s", res.code, res.stderr, res.stdout)
	}

	// A missing file is described with the platform's own error text, so the
	// same capture reads differently on Windows. The goldens are the Linux
	// text; this is the one difference, and it is mapped back here rather than
	// left to fail the Windows CI leg.
	stdout := strings.ReplaceAll(res.stdout, windowsNotExist, "no such file or directory")

	jsonFile := filepath.Join(tree, "golden.json")
	exitFile := filepath.Join(tree, "golden.exit")
	wantExit := strconv.Itoa(res.code) + "\n"
	if *update {
		if err := os.WriteFile(jsonFile, []byte(stdout), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(exitFile, []byte(wantExit), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}

	golden, err := os.ReadFile(jsonFile)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if stdout != string(golden) {
		t.Errorf("stdout differs from %s (run with -update after checking the change is intended)\n%s",
			jsonFile, firstDifference(string(golden), stdout))
	}
	exit, err := os.ReadFile(exitFile)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(exit) != wantExit {
		t.Errorf("exit code = %s, golden %s says %s", strings.TrimSpace(wantExit), exitFile, strings.TrimSpace(string(exit)))
	}
}

// firstDifference names the first line where two outputs diverge.
func firstDifference(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("line %d:\n  golden: %s\n  got:    %s", i+1, wl, gl)
		}
	}
	return "no differing line"
}
