# limits-explain

Show every limit binding a Linux process.

Today it reads the soft and hard resource limits (rlimits) from
`/proc/<pid>/limits` and the memory, task and CPU-bandwidth limits of the
process's cgroup v2 hierarchy, reporting which ancestor actually binds. It
does not yet report usage, rank limits, or read systemd unit directives.

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

A cgroup row names the level that actually binds, which is the thing no
existing tool tells you (one line of the same table, Notes column included):

```text
tasks  cgroup  50  n/a  processes  unmeasured  bound by /le-test (sys/fs/cgroup/le-test/pids.max); leaf /le-test/child allows 100
```

Exit codes:

- `0` a report was produced. It **may be incomplete**: a source that fails on
  its own is reported as a `<source>.source_error` row, with the reason in its
  notes, while the other sources are still shown. Exit `0` does not promise
  that every limit binding the process was read.
- `1` reserved for a future threshold check; never returned today.
- `2` usage error.
- `3` nothing could be reported — the process does not exist, or every source
  failed. Nothing is written to stdout.
- `4` the results could not be written.

## Scope

Linux only: the tool reads `/proc` and `/sys/fs/cgroup`. Cgroups are read
from the v2 unified hierarchy only; a host on cgroup v1 or the hybrid layout
gets a clear error rather than numbers from the wrong hierarchy. Host
processes and system units, not containers.

## Prior art

`below` (source-checked) has no rlimit correlation and no one-shot
limits-vs-usage view — its only rlimit-related code raises its own
`RLIMIT_MEMLOCK` for its BPF collectors. `prlimit(1)`, `systemd-cgtop`, and
`systemctl show` each cover exactly one layer (rlimits, live cgroup usage,
configured directives) with no correlation between them.
