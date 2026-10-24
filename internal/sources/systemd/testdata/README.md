# systemd fixtures

Each directory is a fake root for the sources. Besides the usual `proc/` and
`sys/fs/cgroup/` trees, a case holds `show.txt`: the stdout the fake runner
replays for `systemctl show`. `proc/4242` stands for the process under test.

## Captured from a real host

Captured byte-exact on WSL2 with systemd 259 as PID 1, from transient units
started with `systemd-run --unit=le-<x> --property=... sleep 60`. `show.txt` is
`LC_ALL=C systemctl show --no-pager -p <the properties the code requests> --
<unit>`; `proc/4242/{cgroup,limits}` and the cgroup files are the unit's own
`/proc/<pid>/...` and `/sys/fs/cgroup/...` (root, `system.slice` and the unit
level), read at the same moment. The real pid directory was renamed to
`proc/4242` and nothing else was edited, so `MainPID` in `show.txt` is the
original pid and does not name a directory here.

| case               | what was done to the unit                                          |
|--------------------|--------------------------------------------------------------------|
| `all_match`        | `TasksMax=7 MemoryMax=64M MemoryHigh=32M CPUQuota=50% LimitNOFILE=4096:8192 LimitNPROC=1000` |
| `nofile_infinity`  | `LimitNOFILE=infinity` (the kernel shows 2147483584)               |
| `cpu_fraction`     | `CPUQuota=7.5% CPUQuotaPeriodSec=50ms` (cpu.max `3750 50000`)      |
| `cpu_floored`      | `CPUQuota=33.33% CPUQuotaPeriodSec=7ms` (cpu.max `2333 7000`: systemd floors the quota) |
| `cpu_150`          | `CPUQuota=150%` (`1.500000s`, cpu.max `150000 100000`)             |
| `rlimit_changed`   | `LimitNOFILE=4096:8192`, then `prlimit --pid P --nofile=512:8192`  |
| `pids_overwritten` | `TasksMax=7`, then `echo 5 > .../pids.max`                         |
| `memory_rounded`   | `MemoryMax=10000001 MemoryHigh=10000001` (the kernel holds 9998336)|
| `scope`            | `systemd-run --scope --property=TasksMax=9`: no MainPID line, no Limit lines |
| `not_found`        | `systemctl show -- le-missing.service`: exits 0, LoadState=not-found, defaults. No process tree. |
| `non_systemd`      | a shell's own cgroup on WSL, `0::/non-systemd`, with PID 1 systemd |

Every captured file has LF line endings, and `.gitattributes` marks
`**/testdata/**` as `-text` so git never rewrites them.

## Synthetic

Hand-built, not captured. Each is a copy of `all_match` (or, for the two with
no `show.txt`, of its `proc/` files) with one thing changed.

| case                              | change                                              |
|-----------------------------------|-----------------------------------------------------|
| `synthetic_garbage_line`          | `show.txt`: the `TasksMax=7` line replaced by text with no `=` |
| `synthetic_duplicate_key`         | `show.txt`: `TasksMax=9` appended after `TasksMax=7`|
| `synthetic_user_manager`          | cgroup `.../user@1000.service/app.slice/app-foo.service`; no `show.txt` (it must not be asked) |
| `synthetic_pid1_not_systemd`      | `proc/1/comm` is `init`; no `show.txt` is needed    |
| `synthetic_controlgroup_elsewhere`| `show.txt`: `ControlGroup=/system.slice/other.service`, which does not contain the pid |

The malformed timespans (`1.5us`, `1e3ms`, `-5ms`, `1 s`, overflow), the
unit-name validation cases and the edge cases of the comparison (`max` against
a finite directive, an absent or unparseable row, an ancestor row) are built
inline in the tests, because they are one string each.
