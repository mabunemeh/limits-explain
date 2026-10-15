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
	"github.com/mabunemeh/limits-explain/internal/reporting"
	"github.com/mabunemeh/limits-explain/internal/sources/registry"
)

// Exit codes.
const (
	exitOK = 0
	// exitIO is a failure writing results, such as a closed stdout or a full
	// disk. It is deliberately not 1: exit 1 is reserved for the forthcoming
	// --warn threshold ("a reading is at or above the threshold"), and a
	// wrapper script must never mistake an I/O failure for a limit near its
	// ceiling.
	exitIO = 4
	// exitUsage is a usage error: a non-integer or non-positive pid, a
	// missing pid, an unknown flag or command, or an empty --root.
	exitUsage = 2
	// exitTarget means the target is missing or unreadable: the process
	// directory does not exist under the root, or a source could not read
	// or parse what it needed.
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
// Exit codes: 0 success; 2 usage error (non-integer or non-positive pid,
// missing pid, unknown flag, empty --root); 3 the target is missing or
// unreadable (no such process directory under the root, or any source
// failed, including the process exiting between the existence check and the
// read, or a permission error); 4 failing to write the results. 1 is
// reserved for the forthcoming --warn threshold and is never returned today.
func Execute(args []string, stdout, stderr io.Writer) int {
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
}

func newShowCmd(stdout io.Writer, open rootOpener) *cobra.Command {
	var opts showOptions

	cmd := &cobra.Command{
		Use:   "show <pid>",
		Short: "Show every limit binding pid, with usage where available",
		Long: `Show every limit binding pid, with usage where available.

Exit codes:
  0  success
  2  usage error (missing, non-integer or non-positive pid; unknown flag)
  3  the process does not exist, or its limits could not be read or parsed
  4  the results could not be written (for example a full disk)`,
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
			return runShow(stdout, open(opts.root), opts.root, pid, opts.json)
		},
	}
	cmd.Flags().BoolVar(&opts.json, "json", false, "print machine-readable JSON instead of a table")
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

func runShow(stdout io.Writer, fsys fs.FS, rootLabel string, pid int, asJSON bool) error {
	// The existence check uses the same fs.FS the sources read through, so a
	// fixture tree and the real filesystem behave identically.
	pidDir := fmt.Sprintf("proc/%d", pid)
	if info, err := fs.Stat(fsys, pidDir); err != nil || !info.IsDir() {
		return &exitError{exitTarget, fmt.Errorf(
			"no such process: %d (no %s directory under %s)", pid, pidDir, rootLabel)}
	}

	var readings []model.LimitReading
	for _, src := range registry.All() {
		got, err := src.Collect(pid, fsys)
		if err != nil {
			// Covers a structural parse failure and also the process
			// exiting (or a permission error) between the check above and
			// the source read: both come back as a *SourceError rather
			// than a raw I/O error.
			return &exitError{exitTarget, err}
		}
		readings = append(readings, got...)
	}

	// Everything is collected before anything is printed, so a failure never
	// leaves partial output on stdout.
	render := reporting.WriteTable
	if asJSON {
		render = reporting.WriteJSON
	}
	if err := render(stdout, readings); err != nil {
		return &exitError{exitIO, err}
	}
	return nil
}
