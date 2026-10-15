# limits-explain

Show every limit binding a Linux process.

Today it reads the soft and hard resource limits (rlimits) from
`/proc/<pid>/limits`. It does not yet report usage, rank limits, or read
cgroup or systemd settings.

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

Exit codes: `0` success, `2` usage error, `3` the process does not exist or
its limits could not be read, `4` the results could not be written.

## Scope

Linux only: the tool reads `/proc`. Cgroup data, when it is added, will
target cgroup v2 only.

## Prior art

`below` (source-checked) has no rlimit correlation and no one-shot
limits-vs-usage view — its only rlimit-related code raises its own
`RLIMIT_MEMLOCK` for its BPF collectors. `prlimit(1)`, `systemd-cgtop`, and
`systemctl show` each cover exactly one layer (rlimits, live cgroup usage,
configured directives) with no correlation between them.
