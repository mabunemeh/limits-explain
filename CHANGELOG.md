# Changelog

All notable changes to this project are documented in this file. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- **Exit `1` is now returned.** It means at least one MEASURED reading is at
  or above `--warn` percent of its limit (default 90). The report is printed
  first (table or `--json`) and a one-line message goes to stderr, with no
  usage hint. Unmeasured limits neither trigger nor suppress it: a partial
  report whose measured readings are all under the threshold still exits `0`,
  and its headline names the gap, so exit `0` is never an all-clear on its
  own. A failure to write the report (exit `4`) wins over `1`. Scripts that
  treated every non-zero exit as "the tool broke" must now allow `1`.
- **Table and JSON output changed shape.** The table gains Headroom and Used
  columns after Usage, and a `nearest ceiling:` headline under the title
  (plus a `warning:` line when something is at or above `--warn`). Rows are
  now ranked rather than in collection order, in the table and in `--json`,
  which gains `headroom` and `over_by` (exact integers, `null` when the
  reading is not measured; `headroom` is `0` when usage is at or over the
  limit). The JSON stays a bare array with alphabetical keys and no float
  fields.
- **A source that fails no longer hides the others.** Previously any source
  failure ended the run with exit `3` and nothing on stdout; with a second
  source that would mean a cgroup v1 host losing its rlimit report entirely.
  Now a source that fails on its own is reported as a
  `<source>.source_error` row carrying its status and the reason, the rest of
  the report is printed, and the exit code is `0`. **Exit `0` therefore no
  longer promises a complete report.** Exit `3` now means nothing could be
  reported at all: the process does not exist, or *every* source failed, and
  stdout stays empty. Anything scripted against the previous behaviour should check for
  `source_error` rows (or `status` values of `unreadable` and `unparseable`)
  rather than treating exit `0` as "every limit was read".

### Added

- **Usage and headroom for memory and tasks.** The cgroup source now reads
  `memory.current` (for `memory.max` and `memory.high`) and `pids.current`
  (for `pids.max`), strictly parsed, at every level with a finite limit: each
  level's limit is enforced against its own counter, which covers its whole
  subtree. A missing, unreadable or unparseable counter at ANY finite level
  leaves the reading `unmeasured` (with the tightest limit) and a note naming
  each file and why, because the unknown level could be the one over the
  threshold; usage is never guessed or borrowed. `cpu.max` and the rlimits
  have no usage yet and stay unmeasured.
- **Measured cgroup rows show the level nearest its ceiling.** The displayed
  limit and usage come from the level with the highest exact usage/limit
  (the rootmost on a tie), so a quiet leaf cannot hide an ancestor at 95%.
  The notes name the tightest static ceiling when it is a different level,
  and add `least headroom: <level> (<n> <unit> left)` when another level has
  strictly less absolute headroom (the ratio still decides what is shown).
- **Exact arithmetic.** Headroom never subtracts without comparing first, so
  usage above the limit reports `over by N` instead of wrapping around. The
  `--warn` check (`usage*100 >= percent*limit`) and the ranking comparison run
  in 128 bits with no floating point, so they stay exact above 2^53 and near
  2^64. A zero limit counts as fully consumed at any usage. The displayed
  percentage is floored to a tenth, so a reading that did not trip `--warn 90`
  never displays as `90.0%`.
- **Ranking and headline.** Measured readings by consumed fraction, then the
  readings that could not be measured, then the unlimited ones; ties keep
  collection order. `nearest ceiling:` names the closest one and counts the
  limits not measured.
- **`--warn PERCENT`** (default 90, integer 1-100, plain decimal digits only:
  hex, signs and fractions are usage errors, and `090` is 90, not octal).
- **cgroup v2 source** (`internal/sources/cgroup`): resolves the process's
  cgroup from `proc/<pid>/cgroup`, then walks UP the hierarchy and reports
  the effective limit as the minimum across the leaf and every ancestor,
  naming the level that actually binds and what the leaf alone would have
  allowed. Covers `memory.max`, `pids.max` and `cpu.max` (derived to
  millicores, with the raw quota/period pair in the notes), and reports
  `memory.high` as a separate reading that says plainly it throttles and
  reclaims rather than kills. `max` means unlimited, not a parse failure.
  Values are exact `uint64`. A level whose controller file is absent is
  skipped with a note (normal where a controller is not enabled); a level
  that cannot be read or parsed withholds the limit for that one reading,
  because the bad level could be the binding one. cgroup v1 and the hybrid
  layout, a hierarchy not mounted at `sys/fs/cgroup`, a cgroup directory
  that does not resolve under the mount, and a cgroup path containing `.`
  or `..` are structural `SourceError`s that name what was found, never
  numbers from the wrong hierarchy.
- **rlimits source** (`internal/sources/rlimits`): reads `proc/<pid>/limits`,
  emitting a soft and a hard reading when they differ or one `single`
  reading when they match. Limits are exact `uint64` values, so the kernel
  values above 2^63 (for example `RLIM_INFINITY - 1`) survive intact. Handles
  the kernel's fixed-width column drift (a value that exactly fills its
  field leaves no padding, dropping the usual 2+ space separator to one) on
  either or both sides, accepts CRLF input, rejects indented rows and
  anything but `unlimited` or ASCII digits as a value, keeps unknown kernel
  row names under a slugified key, and degrades a single unparseable row to
  a `status: unparseable` reading instead of aborting the whole read.
  Structural problems (unreadable file, missing header, no data rows) are
  returned as a `SourceError` that wraps the underlying read error, so
  callers can use `errors.Is(err, fs.ErrPermission)` and
  `errors.Is(err, fs.ErrNotExist)`.
- **`limits-explain show <pid> [--json]`**: a table (`text/tabwriter`) or a
  JSON array with a stable, alphabetical key order and `null` for unknown
  values. Unknown limits render as `n/a`; only a reading the kernel reported
  as unlimited renders as `unlimited`. Kernel-derived text is sanitised
  before it reaches the table, so control characters cannot break the
  layout or the terminal. Exit codes: `0` success, `2` usage error
  (including a non-positive pid or an empty `--root`), `3` the process is
  missing or its limits cannot be read, `4` the results could not be
  written (exit `1` was reserved at this point and is now the `--warn` check).
- **`fs.FS` testability seam**: every source reads through an `fs.FS`
  rooted at `/` (or the hidden `--root`), with relative slash-separated
  paths, so a source cannot name a path outside its root. That is a
  guarantee about names, not reads: `os.DirFS` is not a sandbox and does
  follow a symlink pointing out of the tree, so `os.OpenRoot` would be
  needed if `--root` ever became a user-facing feature. Unit tests run
  against committed fixture trees under `internal/sources/rlimits/testdata`
  and `internal/sources/cgroup/testdata`, which are real kernel captures
  plus hand-built edge cases, marked `-text` so Git never rewrites their
  bytes.
- **CI**: gofmt, `go vet`, golangci-lint, build and test with a coverage
  floor of 80% on Linux (with the race detector) and Windows, a static
  `CGO_ENABLED=0` Linux build, and a `systemd-integration` job that spawns a
  transient unit with `systemd-run` on a GitHub Actions runner and reads its
  real rlimits back, de-risking the future systemd source.
