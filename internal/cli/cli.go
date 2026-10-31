// Package cli implements the limits-explain command line.
//
// Nothing in this package calls os.Exit or touches os.Stdout/os.Stderr
// directly: Execute takes its streams as arguments and returns the process
// exit code, so every path can be tested in-process.
package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mabunemeh/limits-explain/internal/model"
	"github.com/mabunemeh/limits-explain/internal/ranking"
	"github.com/mabunemeh/limits-explain/internal/reporting"
	"github.com/mabunemeh/limits-explain/internal/sources"
	"github.com/mabunemeh/limits-explain/internal/sources/fds"
	"github.com/mabunemeh/limits-explain/internal/sources/registry"
	"github.com/mabunemeh/limits-explain/internal/sources/systemd"
)

// Exit codes.
const (
	exitOK = 0
	// exitWarn means the report was produced and at least one MEASURED
	// reading is at or above the --warn threshold. It is returned after the
	// report is already on stdout, and regardless of unmeasured gaps.
	exitWarn = 1
	// exitIO is a failure writing results, such as a closed stdout or a full
	// disk. It is deliberately not 1: exit 1 means "a reading is at or above
	// the threshold", and a wrapper script must never mistake an I/O failure
	// for a limit near its ceiling. A failed write therefore takes precedence
	// over the threshold check.
	exitIO = 4
	// exitUsage is a usage error: a non-integer or non-positive pid, a
	// missing pid, an unknown flag or command, or an empty --root.
	exitUsage = 2
	// exitTarget means nothing could be reported: the process directory
	// does not exist under the root, or EVERY source failed to read or
	// parse what it needed. One source failing while another succeeds is
	// not this code; that report is printed, with the failure as a
	// degraded row, and exits 0 (or 1 if a measured reading is over).
	exitTarget = 3
)

const progName = "limits-explain"

// version is stamped by the release build with
// -ldflags "-X github.com/mabunemeh/limits-explain/internal/cli.version=...".
// An unstamped build falls back to the version Go recorded: the module version
// for `go install ...@v0.1.0`, a commit pseudo-version for `go build` in a git
// checkout. Only a build with neither, such as a test binary, says "dev".
var version = ""

func resolveVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// exitError carries the exit code a command wants. Any error that is not an
// exitError (in practice, the ones cobra itself produces for bad flags and
// arguments) is a usage error.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// rootOpener turns the --root value into the fs.FS every source reads
// through. It is a parameter so tests can wrap the real filesystem with one
// that fails on demand.
type rootOpener func(root string) fs.FS

// runnerOpener chooses how systemd is asked about a unit for the given --root.
// It returns nil when systemd must not be consulted. It is a parameter, like
// rootOpener, so tests can inject a fake that replays captured output.
type runnerOpener func(root string) systemd.Runner

// widthFunc reports the terminal width in columns of the stream the table is
// written to, or 0 when it is not a terminal or the width is unknown. It is
// a parameter, like rootOpener, so tests can set any width without a TTY.
type widthFunc func(w io.Writer) int

// terminalWidth is the real widthFunc: it answers only for an *os.File that
// is a terminal, so piped and redirected output is never truncated.
func terminalWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	fd := int(f.Fd()) //nolint:gosec // file descriptors fit in int
	if !term.IsTerminal(fd) {
		return 0
	}
	width, _, err := term.GetSize(fd)
	if err != nil || width <= 0 {
		return 0
	}
	return width
}

// Execute runs the CLI with the given arguments (without the program name),
// writing results to stdout and errors to stderr, and returns the exit code.
//
// Exit codes: 0 a report was produced and no measured reading is at or above
// --warn (the report MAY be incomplete, see below); 1 a report was produced
// and at least one measured reading is at or above --warn (default 90%), also
// with --json; 2 usage error (non-integer or non-positive pid, missing pid,
// unknown flag, empty --root, --warn outside 1..100); 3 nothing could be
// reported, because the target is missing or unreadable (no such process
// directory under the root, or EVERY source failed, including the process
// exiting between the existence check and the read, or a permission error),
// and nothing is written to stdout; 4 failing to write the results, which
// wins over 1.
//
// Exit 0 does not promise a complete report, and is never an all-clear on its
// own: unmeasured limits are counted in the headline. When some sources fail
// and at least one succeeds, the report is printed and each failed source
// appears as a degraded "<source>.source_error" row carrying its status and
// reason, so the gap is visible in the output rather than only in an exit
// code. Exit 1 is decided by measured readings only, whatever else is
// missing.
func Execute(args []string, stdout, stderr io.Writer) int {
	// os.DirFS stops a source from NAMING a path outside the root, which is
	// what the seam is for, but it is not a chroot-style boundary: it
	// follows a symlink that points out of the root, so a --root aimed at
	// an untrusted tree can still read files outside it. That is acceptable
	// because --root is a hidden testing flag and the real /sys/fs/cgroup
	// holds no symlinks; it would not be if --root ever became a feature.
	return execute(args, stdout, stderr,
		func(root string) fs.FS { return os.DirFS(root) },
		// systemctl talks to the live host, so it is only asked when the
		// sources read the live host too. Under any other --root the systemd
		// step reports that it was not consulted.
		func(root string) systemd.Runner {
			if root == "/" {
				return systemd.ExecRunner{}
			}
			return nil
		},
		terminalWidth)
}

func execute(args []string, stdout, stderr io.Writer, open rootOpener, runner runnerOpener,
	width widthFunc) int {
	if args == nil {
		// cobra falls back to os.Args[1:] for a nil slice. An empty,
		// non-nil slice means "no arguments".
		args = []string{}
	}
	cmd := newRootCmd(stdout, stderr, open, runner, width)
	cmd.SetArgs(args)

	err := cmd.Execute()
	if err == nil {
		return exitOK
	}

	code := exitUsage
	var ee *exitError
	if errors.As(err, &ee) {
		code = ee.code
	}
	// Errors go to stderr only; stdout is reserved for results so machine
	// consumers never see partial or garbage output on a failure path.
	_, _ = fmt.Fprintf(stderr, "%s: %v\n", progName, err)
	// Exit 1 is not a usage problem: the report is already on stdout and the
	// one-line message says why, so no hint to read --help.
	if code == exitUsage {
		_, _ = fmt.Fprintf(stderr, "Try '%s --help' for usage.\n", progName)
	}
	return code
}

func newRootCmd(stdout, stderr io.Writer, open rootOpener, runner runnerOpener, width widthFunc) *cobra.Command {
	root := &cobra.Command{
		Use:   progName,
		Short: "Show every limit binding a Linux process",
		// Cobra would otherwise print its own error and usage text and map
		// every failure to one exit code. Execute does both itself.
		SilenceUsage:  true,
		SilenceErrors: true,
		// Use the root command as a plain container for subcommands.
		// "limits-explain <pid>" is deliberately not accepted: later
		// batches add options to show, such as --unit.
		Version: resolveVersion(),
	}
	root.SetVersionTemplate(progName + " {{.Version}}\n")
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(newShowCmd(stdout, open, runner, width))
	return root
}

type showOptions struct {
	root string
	json bool
	warn string
	unit string
	wide bool
}

// defaultWarn is the --warn percentage when the flag is not given.
const defaultWarn = "90"

func newShowCmd(stdout io.Writer, open rootOpener, runner runnerOpener, width widthFunc) *cobra.Command {
	var opts showOptions

	cmd := &cobra.Command{
		Use:   "show <pid> | --unit NAME",
		Short: "Show every limit binding a process or a systemd unit, with usage where available",
		Long: `Show every limit binding pid, with usage where available.

Give exactly one of <pid> or --unit NAME. --unit resolves a systemd unit to
its main process and then reports that process. When the process belongs to a
system unit, the unit's systemd directives (TasksMax, MemoryMax, MemoryHigh,
CPUQuota, Limit<X>) are added as notes on the cgroup and rlimit rows they set,
and say so when the kernel holds a different value. They never add a row. A
"unit:" line under the headline names the unit, or says why there is none.
A systemctl that fails or prints garbage becomes a "systemd.source_error" row.

Exit codes:
  0  a report was produced and no measured reading is at or above --warn. It
     MAY be incomplete: a source that failed on its own is reported as a
     "<source>.source_error" row, with its status and the reason in the notes,
     while every other source is still reported. Exit 0 therefore does not
     promise that every limit binding the process was read, and it is never
     an all-clear by itself: the "nearest ceiling" line counts the limits
     that were not measured.
  1  at least one MEASURED reading is at or above --warn percent of its
     limit. The report is still printed (table or --json); a one-line message
     goes to stderr. Unmeasured limits do not count toward it, and do not
     suppress it.
  2  usage error (missing, non-integer or non-positive pid; both or neither of
     <pid> and --unit; an invalid unit name; unknown flag; --warn not an
     integer from 1 to 100)
  3  nothing could be reported: the process does not exist, or EVERY source
     failed to read or parse its limits; with --unit, also when systemd is not
     PID 1, the unit is not found, or it has no main process. Nothing is
     written to stdout.
  4  the results could not be written (for example a full disk); this wins
     over 1

Readings are ranked nearest ceiling first.

Table: amounts with the unit "bytes" are shown as B or KiB..EiB with one
decimal, rounded down (JSON keeps exact integers). On a terminal the Notes
column is cut to fit its width, ending in "...", and a last line says so;
--wide shows it in full. Piped output is never cut.

--json prints one object with the keys pid, readings (every reading, exact
integers, null for unknown amounts), summary (nearest: the key of the
nearest-ceiling reading or null; not_measured, total, warned), unit
(directives_read; name and reason, null when not applicable) and warn.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return fmt.Errorf("expected one pid, got %d arguments", len(args))
			}
			byUnit := cmd.Flags().Changed("unit")
			switch {
			case byUnit && len(args) == 1:
				return errors.New("give either a pid or --unit, not both")
			case !byUnit && len(args) == 0:
				return errors.New("missing pid argument (or --unit NAME)")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			byUnit := cmd.Flags().Changed("unit")
			var pid int
			if !byUnit {
				var err error
				if pid, err = parsePID(args[0]); err != nil {
					return err
				}
			} else if err := systemd.ValidateUnitName(opts.unit); err != nil {
				return err
			}
			// An empty --root must be rejected, not quietly treated as the
			// current directory.
			if cmd.Flags().Changed("root") && opts.root == "" {
				return errors.New("--root must not be empty")
			}
			warn, err := parseWarn(opts.warn)
			if err != nil {
				return err
			}
			fsys, run := open(opts.root), runner(opts.root)
			if byUnit {
				if pid, err = systemd.MainPID(run, fsys, opts.unit); err != nil {
					return &exitError{exitTarget, err}
				}
			}
			cols := 0
			if !opts.json && !opts.wide {
				cols = width(stdout)
			}
			return runShow(stdout, fsys, opts.root, pid, opts.json, warn, run, cols)
		},
	}
	cmd.Flags().BoolVar(&opts.wide, "wide", false, "show the Notes column in full instead of cutting it to the terminal width")
	cmd.Flags().StringVar(&opts.unit, "unit", "", "show the main process of the systemd unit `NAME` instead of a pid")
	cmd.Flags().BoolVar(&opts.json, "json", false, "print machine-readable JSON instead of a table")
	// A string flag, not cobra's Uint: pflag parses integers with base 0, so
	// "0x5a" would be accepted and "090" read as octal. parseWarn applies the
	// same strict digits-only rule the sources use.
	cmd.Flags().StringVar(&opts.warn, "warn", defaultWarn,
		"exit 1 when a measured reading is at or above this `PERCENT` of its limit (1-100)")
	// --root is a testing seam (point the sources at a fixture tree), not a
	// user-facing feature.
	cmd.Flags().StringVar(&opts.root, "root", "/", "alternate filesystem root (testing)")
	// MarkHidden fails only for a flag that was never registered, which the
	// line above rules out.
	_ = cmd.Flags().MarkHidden("root")
	return cmd
}

// parsePID accepts only a positive decimal integer. 0 and negative values
// are usage errors, not "no such process".
func parsePID(arg string) (int, error) {
	// Atoi would accept a leading "+"; a pid is plain digits.
	pid, err := strconv.Atoi(arg)
	if err != nil || pid <= 0 || strings.HasPrefix(arg, "+") {
		return 0, fmt.Errorf("invalid pid %q: must be a positive integer", arg)
	}
	return pid, nil
}

// parseWarn accepts only a plain decimal integer from 1 to 100. "", "+90",
// "90.5", "0x5a", "-1", "0" and "101" are usage errors.
func parseWarn(arg string) (uint64, error) {
	bad := fmt.Errorf("invalid --warn %q: must be an integer from 1 to 100", arg)
	if !sources.IsASCIIDigits(arg) {
		return 0, bad
	}
	v, err := strconv.ParseUint(arg, 10, 64)
	if err != nil || v < 1 || v > 100 {
		return 0, bad
	}
	return v, nil
}

func runShow(stdout io.Writer, fsys fs.FS, rootLabel string, pid int, asJSON bool, warn uint64,
	run systemd.Runner, cols int) error {
	// The existence check uses the same fs.FS the sources read through, so a
	// fixture tree and the real filesystem behave identically.
	pidDir := fmt.Sprintf("proc/%d", pid)
	if info, err := fs.Stat(fsys, pidDir); err != nil || !info.IsDir() {
		return &exitError{exitTarget, fmt.Errorf(
			"no such process: %d (no %s directory under %s)", pid, pidDir, rootLabel)}
	}

	// Every source runs even after another has failed. One source erasing
	// the others would answer "nothing" to a question the tool can partly
	// answer (a cgroup v1 host would lose its rlimits), so a failed source
	// contributes a single degraded reading instead, visible in the table
	// and the JSON and not only on stderr.
	var readings []model.LimitReading
	var failures []error
	succeeded := 0
	for _, src := range registry.All() {
		got, err := src.Collect(pid, fsys)
		if err != nil {
			// Covers a structural parse failure and also the process
			// exiting (or a permission error) between the check above and
			// the source read: both come back as a *SourceError rather
			// than a raw I/O error.
			failures = append(failures, err)
			readings = append(readings, sourceFailure(src.Name(), err))
			continue
		}
		readings = append(readings, got...)
		succeeded++
	}

	// With every source down there is nothing to report, so the behaviour
	// is the one a single failing source always had: nothing on stdout, the
	// errors on stderr, exit 3. errors.Join keeps every source's error, not
	// just the first.
	if len(failures) > 0 && succeeded == 0 {
		return &exitError{exitTarget, errors.Join(failures...)}
	}
	// A partial failure is exit 0, not 3, for two reasons. Exit 3 says the
	// TARGET is missing or unreadable, but here the process exists and the
	// report is valid; the gap is visible in the output itself as a
	// degraded row. And exit 1 means a measured reading is at or above the
	// threshold, so it cannot be borrowed to mean "partial" either: the gap
	// is counted in the headline instead.

	// The fd pass fills usage on rows the rlimit source produced, so it runs
	// after the registry loop and before systemd, which then annotates the
	// finished rows. With no matching rows it reads nothing. Its failures
	// are notes, never rows or errors: those rows were already gaps.
	readings = fds.Apply(pid, fsys, readings)

	// systemd only annotates what the other sources found, so it runs last.
	// It adds notes and no rows, except when it fails: then it is one more
	// source_error row, a visible gap like any other source's.
	readings, unitCtx, err := systemd.Consult(run, pid, fsys, readings)
	if err != nil {
		readings = append(readings, sourceFailure(systemd.Name, err))
	}

	// Everything is collected before anything is printed, so a failure never
	// leaves partial output on stdout.
	ranked := ranking.Rank(readings)
	if asJSON {
		err = reporting.WriteJSON(stdout, ranked, reporting.JSONOptions{PID: pid, Warn: warn, Unit: unitCtx})
	} else {
		err = reporting.WriteTable(stdout, ranked, warn, reporting.TableOptions{Width: cols, Context: unitCtx.Line()})
	}
	// A render failure wins over the threshold: the caller must learn the
	// report did not arrive before they learn what it said.
	if err != nil {
		return &exitError{exitIO, err}
	}
	// The report is on stdout; exit 1 is the cron-friendly signal. Only
	// measured readings can trip it. A partial report with nothing measured
	// over the line exits 0, and its headline names the gap.
	if n := ranking.Summarize(ranked, warn).Warned; n > 0 {
		noun := "readings"
		if n == 1 {
			noun = "reading"
		}
		return &exitError{exitWarn, fmt.Errorf("%d %s at or above --warn %d%%", n, noun, warn)}
	}
	return nil
}

// sourceFailure builds the one degraded reading that stands in for a source
// whose Collect failed.
//
// The status follows the SourceError contract: a wrapped read error means the
// input could not be READ (unreadable), while no wrapped error means it was
// read but had no usable structure (unparseable). An error that is not a
// SourceError (no source produces one today) is treated as unreadable, the
// safer claim, since nothing says the input was ever seen. The full error
// text goes in Notes so a message such as the cgroup v1 explanation reaches
// the user verbatim.
func sourceFailure(name string, err error) model.LimitReading {
	status := model.StatusUnreadable
	var se *sources.SourceError
	if errors.As(err, &se) && se.Err == nil {
		status = model.StatusUnparseable
	}
	return model.LimitReading{
		Key:    name + ".source_error",
		Kind:   model.KindSingle,
		Label:  name + " source",
		Source: name,
		Limit:  model.UnknownAmount(),
		Usage:  model.UnknownAmount(),
		Unit:   "",
		Status: status,
		Notes:  err.Error(),
	}
}
