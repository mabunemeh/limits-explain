# How it works

Where each number in a report comes from. Everything is read from `/proc`,
`/sys/fs/cgroup` and, for a unit, one `systemctl show` call. Nothing is
written, and no root is needed for your own processes.

## The root

Every source reads through an `fs.FS` rooted at `/` (`os.DirFS`), with paths
such as `proc/<pid>/limits` and `sys/fs/cgroup/...`. The hidden `--root DIR`
flag points the same code at a fixture tree instead. Under any root other than
`/`, `systemctl` is not run and the `unit:` line says `not consulted under
--root`. `os.DirFS` follows a symlink that leaves the root, so `--root` is a
testing flag, not a sandbox.

## rlimits

`proc/<pid>/limits` gives a soft and a hard value per row. The soft value is
the one the kernel enforces, so it is the one that binds; the hard value is the
ceiling the soft value may be raised to. When they are equal the row is one
reading with key `rlimit.<name>.single`, otherwise two, `.soft` and `.hard`.
`unlimited` is status `unlimited` with no limit. Values are unsigned 64-bit
and parsed strictly (ASCII digits only), so `RLIM_INFINITY - 1` survives. A row
that cannot be parsed is an `unparseable` reading, and the other rows are still
reported.

## cgroup v2 limits

The unified hierarchy is taken to be mounted at `sys/fs/cgroup`. The process's
cgroup is the `0::` line of `proc/<pid>/cgroup`. Four files are read at the
process's cgroup and at every ancestor up to the root: `memory.max`,
`memory.high`, `pids.max` and `cpu.max`. `max` means no ceiling at that level.
A file that exists at no level produces no row.

The kernel enforces every level on its own, so the process is bound by all of
them:

- Where usage can be read (below), the row shows the level with the highest
  usage/limit ratio, compared exactly, the rootmost level on a tie. That can
  be an ancestor with a looser limit than a tighter level that is mostly idle.
  Its notes say `nearest its ceiling: <level>`, and name the tightest limit
  anywhere in the chain (`tightest ceiling`) and any level with less absolute
  headroom (`least headroom`) when they differ.
- Where usage cannot be read, the row shows the tightest limit (the minimum
  over all levels, the leaf-most on a tie) and is `unmeasured`.
- If any level's limit file is unreadable or not understood, the row is
  `unreadable` or `unparseable` with no limit: an unknown level could be the
  binding one, so no minimum of the others is offered.
- All `max` is `unlimited`.

`cpu.max` is shown in millicores (`quota * 1000 / period`, truncated toward
zero; the raw pair is in the notes). Levels are compared on the exact pair, not
on the truncated value. `memory.high` is a separate row because it throttles
and reclaims instead of killing; the OOM ceiling is `memory.max`.

A cgroup v1 or hybrid host produces a single `cgroup.source_error` row that
says so. The rlimit rows are still reported, and the exit code is `0`.

## Usage

- `cgroup.memory.max` and `cgroup.memory.high`: `memory.current`.
- `cgroup.pids.max`: `pids.current`.
- `rlimit.nofile.*`: the number of entries in `proc/<pid>/fd`. The directory is
  listed, never opened per entry. Descriptors numbered at or above the soft
  limit (possible after `prlimit` lowered it) are counted and noted. An
  unreadable directory, as for another user's process, is a note and the rows
  stay unmeasured.
- Counters are read at every level that has a finite limit, because each
  level's limit is enforced against that level's own counter, which counts its
  whole subtree. If any of them cannot be read, the row is unmeasured.
- `cgroup.cpu.max` and the other rlimits have no usage and stay unmeasured.

`rlimit.nproc` is per user, not per process: it counts every task of the real
uid system-wide, and a `/proc` scan would undercount under `hidepid` or a PID
namespace. Its rows stay unmeasured and their notes give this process's thread
count (from `proc/<pid>/status`) and say it is not enforced for uid 0.
`cgroup.pids.max` gets the same thread count as a note. Threads have no row of
their own.

## systemd

For a process in a system unit, the unit's configured directives are added to
the notes of the rows they set. Rows are never added, and no limit, usage or
status changes, so ranking and gap counts are the same with or without it.

The unit is the rootmost `.service` or `.scope` component of the process's
cgroup path, and is looked up only when PID 1 is `systemd`. A process under
`user@N.service` is not read, and neither are directives set on ancestor
slices. The directives come from
`LC_ALL=C systemctl show --no-pager -p <properties> -- <unit>`, with a timeout.
They are trusted only if the unit is `LoadState=loaded` and its `ControlGroup`
contains the process.

| directive                  | row                                       |
|----------------------------|-------------------------------------------|
| `TasksMax`                 | `cgroup.pids.max`                         |
| `MemoryMax`                | `cgroup.memory.max`                       |
| `MemoryHigh`               | `cgroup.memory.high`                      |
| `CPUQuotaPerSecUSec`       | `cgroup.cpu.max`                          |
| `Limit<X>`, `Limit<X>Soft` | `rlimit.<x>.hard`, `.soft` (or `.single`) |

A directive the kernel agrees with is noted as `systemd: TasksMax=7 on
a.service`. One it does not is noted as `systemd: configured TasksMax=7 on
a.service, effective 5`. cgroup directives are compared with the kernel file at
the unit's own level, not at the level the row displays.

A process with no unit gets `unit: none (<reason>)`, which is not an error and
not a gap. A `systemctl` that fails, times out or prints garbage, or a unit
that is not loaded, adds a `systemd.source_error` row.

`show --unit NAME` resolves the unit to its `MainPID` through the same call.
A unit that is not loaded, or has no main process (a scope has no `MainPID`
line), exits `3`; `show <pid>` works for a scope's processes.

## Ranking, the headline and `--warn`

Readings are ordered:

1. Measured readings (limit and usage known), by usage/limit descending,
   compared exactly with 128-bit arithmetic. Ties keep collection order:
   rlimits, then cgroup. A limit of 0 counts as fully consumed.
2. Readings that could not be measured: `unmeasured`, `unreadable`,
   `unparseable` and `source_error` rows.
3. `unlimited` readings.

The headline names the first measured reading as the nearest ceiling. It ends
with `(N limits not measured)` whenever any reading has a status other than
`ok` or `unlimited`, so a report with gaps never reads as an all-clear.

`--warn PERCENT` (an integer from 1 to 100, default 90) decides exit `1`: some
measured reading has `usage * 100 >= PERCENT * limit`. Unmeasured readings
neither trigger nor suppress it. The `Used` column is the percentage floored to
a tenth, so the display is never ahead of the decision. The full list of exit
codes is in the [README](../README.md).

## Fixtures

Unit tests read committed trees, not the host. Per-source fixtures live in the
`testdata/` directory next to each source. Captures of real hosts live in
`internal/cli/testdata/hosts/<case>/`, one directory per host, made by
`scripts/capture-fixture.sh`:

```sh
sudo sh scripts/capture-fixture.sh <pid> <case> [unit]
```

It copies, byte for byte, what the tool reads: `proc/<pid>/{limits,cgroup,status}`
(renamed `proc/4242`), the fd directory as empty files named like the real
entries (no link targets), `proc/1/comm`, `memory.{max,high,current}`,
`pids.{max,current}` and `cpu.{max,stat}` at every cgroup level plus the root
`cgroup.controllers`, and `show.txt` (the `systemctl show` output) when there
is a unit. `host.txt` records the kernel, systemd and OS release and is
documentation only. Review a capture before committing it.

Each case holds `golden.json`, the full stdout of `show 4242 --json --root
<tree>`, and `golden.exit`, the exit code. `TestHosts` walks every directory,
replays `show.txt` in place of `systemctl` (a case with no `show.txt` fails if
systemctl is asked), and compares byte for byte. To add a host, drop in the
capture and run:

```sh
go test ./internal/cli -run Hosts -update
```

then read the diff of the golden files before committing.

The integration tests in `internal/integration` (build tag `integration`) need
systemd as PID 1 and passwordless `sudo`. Each case starts a transient unit
with `systemd-run`, reads it back through the CLI, and stops it.
