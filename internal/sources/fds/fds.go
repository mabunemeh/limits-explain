// Package fds fills in open-file usage on the RLIMIT_NOFILE rows and adds
// thread-count notes to the RLIMIT_NPROC and cgroup pids.max rows.
//
// It is a post-collection pass, not a registry source, because it completes
// rows another source produced. Like the systemd pass it works on a copy of
// the readings, adds notes, and indexes rows by Key. Unlike a source it never
// reports a failure as a source_error row: when /proc/<pid>/fd or
// /proc/<pid>/status cannot be read (another user's process: fd/ is mode
// dr-x------), the rows stay as they were, which already marks them as gaps,
// and a note says why.
package fds

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/mabunemeh/limits-explain/internal/model"
	"github.com/mabunemeh/limits-explain/internal/sources"
)

// Row keys this pass reads and writes. The rlimits source builds them from
// the kernel row name plus the kind.
var (
	nofileKeys = []string{"rlimit.nofile.soft", "rlimit.nofile.hard", "rlimit.nofile.single"}
	nprocKeys  = []string{"rlimit.nproc.soft", "rlimit.nproc.hard", "rlimit.nproc.single"}
)

const (
	pidsKey       = "cgroup.pids.max"
	nofileSoftKey = "rlimit.nofile.soft"
	nofileOnlyKey = "rlimit.nofile.single"
)

// Apply returns a copy of readings in which:
//
//   - the rlimit.nofile rows that have a known limit and no usage carry the
//     number of entries in proc/<pid>/fd as Usage, with Status recomputed;
//   - the rlimit.nproc rows stay unmeasured and gain a note that the limit is
//     per-user, with this process's thread count;
//   - cgroup.pids.max gains a thread-count note.
//
// The input slice and its readings are never modified. When no matching row
// is present, nothing is read and the result is an unchanged copy.
//
// The fd directory is only LISTED with fs.ReadDir. No entry is ever Stat'ed,
// opened or readlinked: the entries are symlinks, and os.DirFS follows a
// symlink that points outside its root. Every entry name must therefore be
// ASCII digits; anything else fails closed (no usage, and a note).
//
// An unlimited nofile row is left completely untouched, as are unreadable and
// unparseable rows: with no ceiling there is nothing to measure against, and
// a failed row's limit is unknown.
func Apply(pid int, fsys fs.FS, readings []model.LimitReading) []model.LimitReading {
	a := &applier{rows: make([]model.LimitReading, len(readings)), index: map[string]int{}}
	copy(a.rows, readings)
	for i, r := range a.rows {
		if r.Status == model.StatusUnreadable || r.Status == model.StatusUnparseable {
			continue
		}
		if _, dup := a.index[r.Key]; !dup {
			a.index[r.Key] = i
		}
	}

	if nofile := a.present(nofileKeys, true); len(nofile) > 0 {
		a.nofile(pid, fsys, nofile)
	}
	nproc := a.present(nprocKeys, false)
	pids, hasPids := a.index[pidsKey]
	if len(nproc) > 0 || hasPids {
		st := readStatus(pid, fsys)
		for _, i := range nproc {
			a.note(i, st.nprocNote())
		}
		if hasPids && st.threadsOK {
			a.note(pids, "this process has "+plural(st.threads, "thread", "threads"))
		}
	}
	return a.rows
}

type applier struct {
	rows  []model.LimitReading
	index map[string]int
}

// present returns the indexes of the rows with the given keys. With
// unmeasuredOnly it keeps just the rows whose limit is known and usage is
// not, which are the only ones this pass may fill.
func (a *applier) present(keys []string, unmeasuredOnly bool) []int {
	var out []int
	for _, k := range keys {
		i, ok := a.index[k]
		if !ok {
			continue
		}
		if unmeasuredOnly && a.rows[i].Status != model.StatusUnmeasured {
			continue
		}
		out = append(out, i)
	}
	return out
}

// note appends text the way the systemd pass does: "; " between notes.
func (a *applier) note(i int, text string) {
	if a.rows[i].Notes != "" {
		a.rows[i].Notes += "; "
	}
	a.rows[i].Notes += text
}

func plural(n uint64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.FormatUint(n, 10) + " " + many
}

// nofile counts the fd directory and fills the given rows.
func (a *applier) nofile(pid int, fsys fs.FS, rows []int) {
	dir := fmt.Sprintf("proc/%d/fd", pid)
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		for _, i := range rows {
			a.note(i, "fd count unreadable: "+sources.Describe(err))
		}
		return
	}
	nums := make([]uint64, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		n, perr := strconv.ParseUint(name, 10, 64)
		if !sources.IsASCIIDigits(name) || perr != nil {
			for _, i := range rows {
				a.note(i, "fd count not used: the fd directory had an unexpected entry")
			}
			return
		}
		nums = append(nums, n)
	}

	count := model.KnownAmount(uint64(len(nums)))
	// prlimit can lower the soft limit below descriptors that are already
	// open. Those are counted (they are open) but the count then overstates
	// how close the next open() is, so say so.
	var soft model.Amount
	for _, k := range []string{nofileSoftKey, nofileOnlyKey} {
		if i, ok := a.index[k]; ok {
			soft = a.rows[i].Limit
			break
		}
	}
	var above uint64
	if soft.Known {
		for _, n := range nums {
			if n >= soft.Value {
				above++
			}
		}
	}
	for _, i := range rows {
		a.rows[i].Usage = count
		a.rows[i].Status = model.DefaultStatus(a.rows[i].Limit, count)
		if above > 0 {
			verb, noun := "are", "descriptors"
			if above == 1 {
				verb, noun = "is", "descriptor"
			}
			a.note(i, fmt.Sprintf("%d open %s %s numbered at or above the soft limit", above, noun, verb))
		}
	}
}

// procStatus is the part of proc/<pid>/status this pass reads. Each field is
// valid only when its OK flag is set; nothing is guessed.
type procStatus struct {
	threads   uint64
	threadsOK bool
	uid       uint64
	uidOK     bool
	readErr   error
}

// readStatus parses the "Threads:" and "Uid:" lines strictly. Both are
// tab-separated; Uid has four fields (real, effective, saved, fs) and the
// REAL uid, the first, is what RLIMIT_NPROC is counted against. A missing or
// malformed line leaves its field unset.
func readStatus(pid int, fsys fs.FS) procStatus {
	var st procStatus
	data, err := fs.ReadFile(fsys, fmt.Sprintf("proc/%d/status", pid))
	if err != nil {
		st.readErr = err
		return st
	}
	seenThreads, seenUID := false, false
	for _, line := range sources.SplitLines(string(data)) {
		if rest, ok := strings.CutPrefix(line, "Threads:\t"); ok {
			if seenThreads {
				continue
			}
			seenThreads = true
			if v, ok := parseDigits(rest); ok {
				st.threads, st.threadsOK = v, true
			}
		} else if rest, ok := strings.CutPrefix(line, "Uid:\t"); ok {
			if seenUID {
				continue
			}
			seenUID = true
			fields := strings.Split(rest, "\t")
			if len(fields) != 4 {
				continue
			}
			if v, ok := parseDigits(fields[0]); ok {
				st.uid, st.uidOK = v, true
			}
		}
	}
	return st
}

func parseDigits(s string) (uint64, bool) {
	if !sources.IsASCIIDigits(s) {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	return v, err == nil
}

// nprocNote explains why the NPROC rows stay unmeasured.
//
// RLIMIT_NPROC is checked per real uid against every task that uid owns
// system-wide, so this process's own thread count is not its usage. A
// /proc-wide scan to total it would silently undercount under hidepid or a
// PID namespace, so no usage is invented.
func (st procStatus) nprocNote() string {
	who := "the real uid"
	if st.uidOK {
		who = "real uid " + strconv.FormatUint(st.uid, 10)
	}
	text := "per-user limit: counts every task of " + who + " system-wide, not just this process"
	switch {
	case st.readErr != nil:
		text += "; process status unreadable: " + sources.Describe(st.readErr)
	case st.threadsOK:
		text += "; this process has " + plural(st.threads, "thread", "threads")
	default:
		text += "; thread count unreadable"
	}
	// The kernel (copy_process) skips the check for real uid 0 (INIT_USER),
	// and for any process holding CAP_SYS_RESOURCE or CAP_SYS_ADMIN. Only the
	// uid is visible here without parsing capability masks, so the note
	// claims only the uid-0 case.
	if st.uidOK && st.uid == 0 {
		text += "; not enforced for uid 0"
	}
	return text
}
