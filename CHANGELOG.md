# Changelog

All notable changes to this project are documented in this file. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

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
  written (exit `1` is reserved for a future threshold check).
- **`fs.FS` testability seam**: every source reads through an `fs.FS`
  rooted at `/` (or the hidden `--root`), with relative slash-separated
  paths, so a source cannot escape its root. Unit tests run against
  committed fixture trees under `internal/sources/rlimits/testdata`, which
  are real kernel captures plus hand-built edge cases, marked `-text` so
  Git never rewrites their bytes.
- **CI**: gofmt, `go vet`, golangci-lint, build and test with a coverage
  floor of 80% on Linux (with the race detector) and Windows, a static
  `CGO_ENABLED=0` Linux build, and a `systemd-integration` job that spawns a
  transient unit with `systemd-run` on a GitHub Actions runner and reads its
  real rlimits back, de-risking the future systemd source.
