// Package sources defines what a source of limit readings is and how
// sources fail. Concrete sources live in subpackages; the list of sources
// the CLI runs lives in the registry subpackage.
package sources

import (
	"io/fs"

	"github.com/mabunemeh/limits-explain/internal/model"
)

// Source is a check that reads one category of limit for a process.
type Source interface {
	// Name is a short identifier such as "rlimit". It is also the value of
	// the Source field of every reading the source produces.
	Name() string

	// Collect returns every limit reading the source can find for pid.
	//
	// fsys is THE testability seam. Every filesystem read in every source
	// goes through it, using relative, slash-separated paths such as
	// "proc/4242/limits". The CLI passes os.DirFS("/") (or the --root
	// directory) and tests pass a directory of fixtures. Because an fs.FS
	// cannot NAME anything outside its root (fs.ValidPath rejects rooted
	// paths and ".." elements), a source cannot reach outside by
	// constructing a path, even by mistake; there is no convention to
	// remember.
	//
	// That is a guarantee about names, not about reads, and the difference
	// is load-bearing. os.DirFS is not a sandbox: it opens root + name with
	// the operating system, so a SYMLINK inside the tree that points
	// outside it is followed, and the content of a file outside the root
	// can come back. Measured on go1.27.2: a symlinked directory component
	// resolved to /etc and its contents were read. The real /sys/fs/cgroup
	// has no symlinks and --root is a hidden testing flag, so this costs
	// nothing today, but it matters for anything reading proc/<pid>/fd,
	// cwd, exe or root, which ARE symlinks and usually point outside any
	// fixture tree. If --root ever becomes a user-facing feature, or a
	// source must follow those links safely, os.OpenRoot (os.Root) is the
	// real boundary; os.DirFS cannot be made into one.
	//
	// A structural failure (the input cannot be read, or has no usable
	// structure) is returned as a *SourceError. A single bad row must
	// instead degrade to a reading with an explicit status, so one bad row
	// never hides every other reading.
	Collect(pid int, fsys fs.FS) ([]model.LimitReading, error)
}

// SourceError is a structural failure a source cannot recover from for the
// whole input.
//
// It is reserved for failures that make the rest of the input meaningless:
// the file could not be read at all (missing, permission denied, process
// exited mid-read), the header is missing or wrong, or the file has no data
// rows. A single unparseable row is NOT a SourceError; it degrades to a
// [model.LimitReading] with [model.StatusUnparseable]. The rule is to fail
// open on individual rows and fail closed on structure: never guess.
//
// The error message names the offending path, so a failure points straight
// at the file that broke parsing.
//
// Read failures wrap the underlying error. Callers tell permission from
// absence with errors.Is(err, fs.ErrPermission) and
// errors.Is(err, fs.ErrNotExist) rather than inspecting an errno, which is
// not portable and cannot be produced by an fs.FS test double on every
// platform.
type SourceError struct {
	// Path is the slash-separated path within the fs.FS that failed.
	Path string
	// Detail says what was wrong, for example "unreadable: permission
	// denied" or "no data rows".
	Detail string
	// Err is the wrapped read error. It is nil for structural parse
	// failures such as a bad header.
	Err error
}

// Error formats the error as "<path>: <detail>".
func (e *SourceError) Error() string {
	return e.Path + ": " + e.Detail
}

// Unwrap returns the wrapped read error, or nil.
func (e *SourceError) Unwrap() error {
	return e.Err
}
