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
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mabunemeh/limits-explain/internal/model"
	"github.com/mabunemeh/limits-explain/internal/ranking"
	"github.com/mabunemeh/limits-explain/internal/reporting"
	"github.com/mabunemeh/limits-explain/internal/sources"
	"github.com/mabunemeh/limits-explain/internal/sources/registry"
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
	return execute(args, stdout, stderr, func(root string) fs.FS { return os.DirFS(root) })
}

func execute(args []string, stdout, stderr io.Writer, open rootOpener) int {
	if args == nil {
		// cobra falls back to os.Args[1:] for a nil slice. An empty,
		// non-nil slice means "no arguments".
		args = []string{}
	}
	cmd := newRootCmd(stdout, stderr, open)
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

func newRootCmd(stdout, stderr io.Writer, open rootOpener) *cobra.Command {
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
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(newShowCmd(stdout, open))
	return root
}

type showOptions struct {
	root string
	json bool
	warn string
}

// defaultWarn is the --warn percentage when the flag is not given.
const defaultWarn = "90"

func newShowCmd(stdout io.Writer, open rootOpener) *cobra.Command {
	var opts showOptions

	cmd := &cobra.Command{
		Use:   "show <pid>",
		Short: "Show every limit binding pid, with usage where available",
		Long: `Show every limit binding pid, with usage where available.

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
  2  usage error (missing, non-integer or non-positive pid; unknown flag;
     --warn not an integer from 1 to 100)
  3  nothing could be reported: the process does not exist, or EVERY source
     failed to read or parse its limits. Nothing is written to stdout.
  4  the results could not be written (for example a full disk); this wins
     over 1

Readings are ranked nearest ceiling first.`,
		Args: func(_ *cobra.Command, args []string) error {
			switch {
			case len(args) == 0:
				return errors.New("missing pid argument")
			case len(args) > 1:
				return fmt.Errorf("expected one pid, got %d arguments", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, err := parsePID(args[0])
			if err != nil {
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
			return runShow(stdout, open(opts.root), opts.root, pid, opts.json, warn)
		},
	}
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

func runShow(stdout io.Writer, fsys fs.FS, rootLabel string, pid int, asJSON bool, warn uint64) error {
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

	// Everything is collected before anything is printed, so a failure never
	// leaves partial output on stdout.
	ranked := ranking.Rank(readings)
	var err error
	if asJSON {
		err = reporting.WriteJSON(stdout, ranked)
	} else {
		err = reporting.WriteTable(stdout, ranked, warn)
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
