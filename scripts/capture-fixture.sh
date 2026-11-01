#!/bin/sh
# capture-fixture.sh - copy the files limits-explain reads for one process
# into a fake root, for use as a test fixture.
#
#   sudo sh capture-fixture.sh <pid> <case-name> [unit]
#
# Writes ./<case-name>/ and ./<case-name>.tar.gz. Files are copied byte for
# byte with cat; the only change is that proc/<pid> is renamed proc/4242.
# The fd directory is recorded as empty files named like the real entries
# (the tool only counts them; link targets are never copied). If <unit> is
# given, or the process's cgroup ends in a .service or .scope, show.txt holds
# `systemctl show` for it with the exact properties the tool asks for.
# host.txt records the kernel, systemd and OS release; no hostname, IP or
# environment is read. Review the tree before sharing it.
set -eu

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
	echo "usage: sudo sh $0 <pid> <case-name> [unit]" >&2
	exit 2
fi
pid=$1
name=$2
unit=${3:-}

case $pid in '' | *[!0-9]*)
	echo "pid must be a number: $pid" >&2
	exit 2
	;;
esac
case $name in '' | *[!A-Za-z0-9_-]*)
	echo "case name may only hold letters, digits, - and _: $name" >&2
	exit 2
	;;
esac
[ -d "/proc/$pid" ] || {
	echo "no such process: $pid" >&2
	exit 1
}
[ ! -e "$name" ] || {
	echo "$name already exists" >&2
	exit 1
}

out=$name
proc=$out/proc/4242
mkdir -p "$proc/fd" "$out/proc/1"
missing=""

# copy SRC DEST: byte-exact, and remember anything unreadable.
copy() {
	if [ -e "$1" ]; then
		if ! cat "$1" >"$2" 2>/dev/null; then
			rm -f "$2"
			missing="$missing $1"
		fi
	fi
}

copy /proc/1/comm "$out/proc/1/comm"
for f in limits cgroup status; do
	copy "/proc/$pid/$f" "$proc/$f"
done
for fd in /proc/"$pid"/fd/*; do
	[ -e "$fd" ] || [ -L "$fd" ] || continue
	: >"$proc/fd/${fd##*/}"
done

# Walk the unified (v2) path from the root down to the process's cgroup.
cgpath=$(sed -n 's/^0:://p' "/proc/$pid/cgroup")
root=/sys/fs/cgroup
mkdir -p "$out$root"
copy "$root/cgroup.controllers" "$out$root/cgroup.controllers"
if [ -n "$cgpath" ] && [ -f "$root/cgroup.controllers" ]; then
	level=""
	rest=${cgpath#/}
	while :; do
		dir=$root$level
		mkdir -p "$out$dir"
		for f in memory.max memory.high memory.current pids.max pids.current cpu.max cpu.stat; do
			copy "$dir/$f" "$out$dir/$f"
		done
		[ -n "$rest" ] || break
		seg=${rest%%/*}
		level=$level/$seg
		case $rest in */*) rest=${rest#*/} ;; *) rest="" ;; esac
	done
fi

if [ -z "$unit" ]; then
	leaf=${cgpath##*/}
	case $leaf in *.service | *.scope) unit=$leaf ;; esac
fi
if [ -n "$unit" ] && [ "$(cat /proc/1/comm)" = systemd ]; then
	props=Id,LoadState,ActiveState,MainPID,ControlGroup,TasksMax,MemoryMax,MemoryHigh,CPUQuotaPerSecUSec,CPUQuotaPeriodUSec
	for l in CPU FSIZE DATA STACK CORE RSS NOFILE AS NPROC MEMLOCK LOCKS SIGPENDING MSGQUEUE NICE RTPRIO RTTIME; do
		props=$props,Limit$l,Limit${l}Soft
	done
	LC_ALL=C systemctl show --no-pager -p "$props" -- "$unit" >"$out/show.txt"
fi

{
	echo "kernel: $(uname -r)"
	echo "systemd: $(systemctl --version 2>/dev/null | head -n 1)"
	echo "os: $(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME")"
	echo "unit: ${unit:-none}"
	echo "cgroup: ${cgpath:-none (no unified entry)}"
	echo "unreadable:${missing:- none}"
} >"$out/host.txt"

tar czf "$out.tar.gz" "$out"
echo "wrote $out/ and $out.tar.gz"
cat "$out/host.txt"
