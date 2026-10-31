package cli

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mabunemeh/limits-explain/internal/sources/systemd"
)

// runAtWidth runs the CLI as if stdout were a terminal of the given width.
func runAtWidth(width int, args ...string) result {
	var stdout, stderr bytes.Buffer
	code := execute(args, &stdout, &stderr,
		func(root string) fs.FS { return os.DirFS(root) },
		func(string) systemd.Runner { return nil },
		func(io.Writer) int { return width })
	return result{code, stdout.String(), stderr.String()}
}

func TestNotesTruncateOnATerminalAndWideShowsThem(t *testing.T) {
	root := cgroupRoot(t, "usage_ratio_vs_headroom")
	args := []string{"show", "4242", "--root", root}
	full := runAtWidth(0, args...)
	if full.code != 1 {
		t.Fatalf("exit %d, stderr: %s", full.code, full.stderr)
	}
	if strings.Contains(full.stdout, "truncated") {
		t.Fatalf("an unknown width truncated:\n%s", full.stdout)
	}

	tests := []struct {
		name string
		args []string
		cols int
		cut  bool
	}{
		{"piped: width unknown", args, 0, false},
		{"terminal, narrow", args, 100, true},
		{"terminal, below the minimum", args, 10, true},
		{"terminal, huge", args, 100000, false},
		{"wide on a terminal", append([]string{"show", "--wide"}, args[1:]...), 100, false},
		{"wide after the command", append(args, "--wide"), 100, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runAtWidth(tc.cols, tc.args...)
			if res.code != 1 {
				t.Fatalf("exit %d, stderr: %s", res.code, res.stderr)
			}
			if got := strings.Contains(res.stdout, "notes truncated to fit "+strconv.Itoa(tc.cols)+" columns; --wide shows them in full\n"); got != tc.cut {
				t.Errorf("truncation line present = %v, want %v:\n%s", got, tc.cut, res.stdout)
			}
			if !tc.cut && res.stdout != full.stdout {
				t.Errorf("output differs from the untruncated report:\n%s", res.stdout)
			}
			if tc.cut {
				if !strings.HasSuffix(res.stdout, "--wide shows them in full\n") {
					t.Errorf("truncation line is not last:\n%s", res.stdout)
				}
				// The headline is never cut, and exit codes do not change.
				if strings.Split(res.stdout, "\n")[1] != strings.Split(full.stdout, "\n")[1] {
					t.Errorf("headline changed:\n%s", res.stdout)
				}
			}
		})
	}

	// At width 100 every table line but the minimum-notes overflow fits.
	res := runAtWidth(100, args...)
	for _, l := range strings.Split(res.stdout, "\n") {
		if strings.HasPrefix(l, "memory") && utf8.RuneCountInString(l) > 100 {
			t.Errorf("row is %d columns wide at width 100: %q", utf8.RuneCountInString(l), l)
		}
	}
}

func TestJSONIsNeverTruncated(t *testing.T) {
	root := cgroupRoot(t, "usage_ratio_vs_headroom")
	wide := runAtWidth(0, "show", "4242", "--root", root, "--json")
	narrow := runAtWidth(20, "show", "4242", "--root", root, "--json")
	both := runAtWidth(20, "show", "4242", "--root", root, "--json", "--wide")
	if wide.stdout != narrow.stdout || wide.stdout != both.stdout {
		t.Errorf("--json depends on the terminal width or --wide")
	}
	if strings.Contains(narrow.stdout, "...\"") || strings.Contains(narrow.stdout, "truncated") {
		t.Errorf("--json was truncated:\n%s", narrow.stdout)
	}
}

func TestJSONEnvelope(t *testing.T) {
	root := cgroupRoot(t, "usage_ratio_vs_headroom")
	res := run("show", "4242", "--root", root, "--json", "--warn", "80")
	if res.code != 1 {
		t.Fatalf("exit %d, stderr: %s", res.code, res.stderr)
	}
	env := decodeEnvelope(t, res.stdout)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	if len(keys) != 5 {
		t.Errorf("envelope keys = %v, want pid, readings, summary, unit, warn", keys)
	}
	if env["pid"].(interface{ String() string }).String() != "4242" ||
		env["warn"].(interface{ String() string }).String() != "80" {
		t.Errorf("pid/warn = %v/%v", env["pid"], env["warn"])
	}

	sum := env["summary"].(map[string]any)
	if sum["nearest"] != "cgroup.memory.max" {
		t.Errorf("summary.nearest = %v, want the key of the nearest reading", sum["nearest"])
	}
	for _, k := range []string{"not_measured", "total", "warned"} {
		if _, ok := sum[k]; !ok {
			t.Errorf("summary lacks %q", k)
		}
	}
	if sum["warned"].(interface{ String() string }).String() != "1" {
		t.Errorf("summary.warned = %v, want 1", sum["warned"])
	}

	// Without systemd the unit has no name and says why.
	unit := env["unit"].(map[string]any)
	if unit["name"] != nil || unit["directives_read"] != false ||
		unit["reason"] != systemd.ReasonNotConsulted {
		t.Errorf("unit = %v", unit)
	}
	// The unit line is a table concern only.
	if strings.Contains(res.stdout, "unit: ") {
		t.Errorf("the unit line leaked into the JSON:\n%s", res.stdout)
	}
}

func TestJSONEnvelopeUnitRead(t *testing.T) {
	root := unitRoot(t, "all_match")
	res := runWithRunner(replayOf(t, "all_match"), "show", "--unit", "le-match.service", "--root", root, "--json")
	if res.code != 0 {
		t.Fatalf("exit %d, stderr: %s", res.code, res.stderr)
	}
	unit := decodeEnvelope(t, res.stdout)["unit"].(map[string]any)
	if unit["name"] != "le-match.service" || unit["directives_read"] != true || unit["reason"] != nil {
		t.Errorf("unit = %v", unit)
	}
}

func TestTerminalWidthIsZeroWithoutATerminal(t *testing.T) {
	if got := terminalWidth(&bytes.Buffer{}); got != 0 {
		t.Errorf("a buffer has width %d, want 0", got)
	}
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := terminalWidth(f); got != 0 {
		t.Errorf("a regular file has width %d, want 0", got)
	}
}

func TestHelpDocumentsWideAndTheEnvelope(t *testing.T) {
	res := run("show", "--help")
	for _, want := range []string{"--wide", "summary", "directives_read", "nearest", "KiB"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("show help lacks %q:\n%s", want, res.stdout)
		}
	}
}
