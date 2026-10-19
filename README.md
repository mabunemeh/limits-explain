# limits-explain

Show every limit binding a Linux process.

It reads the soft and hard resource limits (rlimits) from `/proc/<pid>/limits`
and the memory, task and CPU-bandwidth limits of the process's cgroup v2
hierarchy, reporting which ancestor actually binds.

## Install

```sh
go install github.com/mabunemeh/limits-explain/cmd/limits-explain@latest
```

Requires Go 1.25 or newer.

## Usage

```sh
limits-explain show <pid>          # table
limits-explain show <pid> --json   # machine-readable
```

```text
limits-explain
Name                   Source  Limit      Usage  Unit       Status      Notes
cpu time               rlimit  unlimited  n/a    seconds    unlimited
stack size (soft)      rlimit  8388608    n/a    bytes      unmeasured
stack size (hard)      rlimit  unlimited  n/a    bytes      unlimited
processes              rlimit  62339      n/a    processes  unmeasured
open files (soft)      rlimit  10240      n/a    files      unmeasured
open files (hard)      rlimit  1048576    n/a    files      unmeasured
```

The Usage column is always `n/a`; the tool reports limits only.

A cgroup row names the level that actually binds, in the Notes column:

```text
tasks  cgroup  50  n/a  processes  unmeasured  bound by /le-test (sys/fs/cgroup/le-test/pids.max); leaf /le-test/child allows 100
```

Exit codes:

- `0` a report was produced. It may be incomplete: a source that fails on its
  own is reported as a `<source>.source_error` row, with the reason in its
  notes, while the other sources are still shown. Exit `0` does not promise
  that every limit binding the process was read.
- `1` reserved for a future threshold check; never returned today.
- `2` usage error.
- `3` nothing could be reported, because the process does not exist or every
  source failed. Nothing is written to stdout.
- `4` the results could not be written.

## Scope

Linux only: the tool reads `/proc` and `/sys/fs/cgroup`. Cgroups are read from
the v2 unified hierarchy only. On a cgroup v1 or hybrid host the cgroup limits
show as a single `cgroup.source_error` row and the rlimits are still reported.
Host processes and system units, not containers.

## Alternatives

Use `prlimit(1)` to read or set rlimits on their own, `systemd-cgtop` or
`below` to watch cgroup usage over time, and `systemctl show` for a unit's
configured directives.

## Development

```sh
go test ./...                                       # unit tests against fixture trees
go test -tags=integration ./internal/integration/   # needs systemd as PID 1
golangci-lint run
```

## License

Apache-2.0.
