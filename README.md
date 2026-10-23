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
nearest ceiling: memory (high) 77.5% (620 of 800 bytes) (10 limits not measured)
Name               Source  Limit    Usage  Headroom  Used   Unit       Status
memory (high)      cgroup  800      620    180       77.5%  bytes      ok
memory             cgroup  1000     620    380       62.0%  bytes      ok
tasks              cgroup  100      9      91        9.0%   processes  ok
stack size (soft)  rlimit  8388608  n/a    n/a       n/a    bytes      unmeasured
...
```

Readings are ranked nearest ceiling first, then the limits that could not be
measured, then the unlimited ones. Usage is read for the cgroup memory and
task limits, at every level that has one; the rlimit rows are limits only for now and count as not
measured. The headline names how many were not measured whenever any were, so a report is
never an all-clear on its own. `--warn PERCENT` (default 90, 1-100) sets the
threshold for exit `1`.

A cgroup row shows the level nearest its ceiling (by usage/limit, the
rootmost on a tie), which can be an ancestor looser than the tightest limit,
and says so in the Notes column:

```text
memory  cgroup  100000  90000  10000  90.0%  bytes  ok  nearest its ceiling: /a (sys/fs/cgroup/a/memory.max); usage read at /a (sys/fs/cgroup/a/memory.current), which counts its whole subtree; tightest ceiling: /a/b allows 1000; least headroom: /a/b (500 bytes left); ...
```

If any level's usage counter cannot be read the row stays unmeasured.

Exit codes:

- `0` a report was produced and no measured reading is at or above `--warn`.
  It may be incomplete: a source that fails on its
  own is reported as a `<source>.source_error` row, with the reason in its
  notes, while the other sources are still shown. Exit `0` does not promise
  that every limit binding the process was read.
- `1` a measured reading is at or above `--warn`. The report is still
  printed; unmeasured limits neither trigger nor suppress it.
- `2` usage error.
- `3` nothing could be reported, because the process does not exist or every
  source failed. Nothing is written to stdout.
- `4` the results could not be written; this wins over `1`.

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
