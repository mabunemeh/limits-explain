# Changelog

All notable changes to this project are documented in this file. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-11-02

First release.

### Added

- **`limits-explain show <pid>`** reports every limit binding a Linux
  process, and **`show --unit NAME`** does the same for a systemd unit's main
  process. Exactly one of the two is required. A unit name may contain
  letters, digits and `: _ . @ - \`, at most 255 bytes, and must not start
  with `-`.
- **rlimits** from `/proc/<pid>/limits`: one `single` reading when soft and
  hard are equal, otherwise a `soft` and a `hard` reading. Values are exact
  `uint64`, so kernel values above 2^63 (such as `RLIM_INFINITY - 1`) are
  kept intact. The parser handles the kernel's column drift when a value
  fills its whole field, and accepts CRLF. Unknown row names are kept under
  a slugified key. A row it cannot parse becomes an `unparseable` reading
  instead of failing the whole file.
- **cgroup v2 limits**: `memory.max`, `memory.high` (reported separately,
  because it throttles and reclaims instead of killing), `pids.max`, and
  `cpu.max` (in millicores, with the raw quota and period in the notes). The
  process's cgroup is walked up to the root, and every level counts. A level
  without the controller file is skipped with a note. A level that cannot be
  read or parsed withholds that reading, since it could be the binding one.
  cgroup v1, hybrid layouts and paths that do not resolve under
  `/sys/fs/cgroup` are reported as errors, never as numbers from the wrong
  hierarchy.
- **Usage and headroom.** Memory and task limits are checked against each
  level's own `memory.current` or `pids.current`. A row shows the level
  nearest its ceiling, and the rootmost level wins a tie, so a quiet leaf
  cannot hide a parent at 95%. Notes name the tightest static limit when it
  is on a different level, and the level with the least absolute headroom
  when that differs. If any level's counter cannot be read, the row is
  `unmeasured`; usage is never guessed. Usage above the limit is shown as
  `over by N`.
- **Open files and threads.** The open-files rows count the entries in
  `/proc/<pid>/fd`; that directory is only listed, never followed. An entry
  name that is not all digits leaves the row unmeasured, with a note. The
  `nproc` rows stay unmeasured, because the limit counts every task of the
  user, not this process. Their note gives the real uid and this process's
  thread count, and says the limit is not enforced for uid 0. `pids.max`
  notes the thread count as well.
- **systemd directives as notes.** For a process in a system unit,
  directives such as `TasksMax`, `MemoryMax`, `MemoryHigh`, `CPUQuota` and
  `Limit*` are added to the notes of the rows they set, with the effective
  kernel value when it differs. Page rounding of memory limits and
  whole-microsecond flooring of CPU quotas are recognised and said so.
  Directives never add rows or change numbers. `systemctl show` is the only
  command run, with `LC_ALL=C`, a 10 second timeout and a validated unit
  name, and its output is parsed fail-closed. Processes under
  `user@N.service` and directives on ancestor slices are not read.
- **Ranking and `--warn`.** Readings are ordered by how close they are to
  their limit, then the unmeasured ones, then the unlimited ones. A
  `nearest ceiling:` headline names the closest reading and counts the
  limits not measured, so a report never reads as an all-clear on its own.
  `--warn PERCENT` (default 90, integer 1-100) sets the threshold for exit
  `1`; unmeasured readings neither trigger nor suppress it. Comparisons are
  exact 128-bit integer arithmetic with no floating point. The displayed
  percentage is floored to a tenth, so a reading under the threshold never
  shows as reaching it.
- **Partial reports.** A source that fails on its own becomes one
  `<source>.source_error` row with the reason, and the other sources are
  still reported. Exit `0` therefore means a report was produced, not that
  every limit was read.
- **Table output**: kernel text is sanitised before it reaches the terminal.
  Byte amounts are shown as `B` or `KiB` to `EiB` with one decimal, rounded
  down. On a terminal the Notes column is cut to fit and a last line says
  so; `--wide` shows it in full. Piped output is never cut. A `unit:` line
  names the process's unit, or says why there is none.
- **`--json`**: one object with alphabetical keys: `pid`, `readings`,
  `summary` (`nearest`, `not_measured`, `total`, `warned`), `unit`
  (`directives_read`, `name`, `reason`) and `warn`. Each reading carries
  `key`, `kind`, `label`, `name`, `source`, `status`, `unit`, `notes`, and
  `limit`, `usage`, `headroom` and `over_by` as exact integers or `null`.
- **Exit codes**: `0` report produced, `1` a measured reading is at or above
  `--warn`, `2` usage error, `3` nothing could be reported (no such process,
  every source failed, or for `--unit`: systemd is not PID 1, or the unit
  is not loaded or has no main process), `4` the output could not be
  written (this wins over `1`).
- **`--version`**, stamped with the tag in release builds.
- **Releases**: static (`CGO_ENABLED=0`) linux amd64 and arm64 tarballs with
  `checksums.txt`, built by goreleaser on each `v*` tag. The release notes
  are this file's section for the tag.
- **Tests and CI**: unit tests against fixture trees captured from real
  kernels plus hand-built edge cases, golden `--json` output for real-host
  captures (`scripts/capture-fixture.sh`), and `systemd-run` integration
  tests on `ubuntu-24.04` and `ubuntu-latest`. CI runs gofmt, `go vet`,
  golangci-lint, and the tests on Linux (with the race detector) and
  Windows. [docs/how-it-works.md](docs/how-it-works.md) explains where each
  number comes from.

[Unreleased]: https://github.com/mabunemeh/limits-explain/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/mabunemeh/limits-explain/releases/tag/v0.1.0
