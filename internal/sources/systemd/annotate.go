package systemd

import (
	"fmt"
	"io/fs"
	"math/bits"
	"strconv"

	"github.com/mabunemeh/limits-explain/internal/model"
	"github.com/mabunemeh/limits-explain/internal/sources"
	"github.com/mabunemeh/limits-explain/internal/sources/cgroup"
)

// pageSizes are the Linux page sizes a memory limit can be rounded to. The
// kernel rounds memory.max and memory.high DOWN to a whole page when they are
// written (observed on a real host: 1000001 became 999424, 244 pages of 4096),
// but nothing readable through the fs.FS says which page size the kernel
// uses. A file value is therefore taken as a rounded form of the directive
// only when it is below it, by less than one page, for a page size that
// divides it; the note states exactly that and does not name a page size.
var pageSizes = []uint64{4096, 16384, 65536}

func roundedDown(directive, file uint64) bool {
	if file >= directive {
		return false
	}
	for _, p := range pageSizes {
		if file%p == 0 && directive-file < p {
			return true
		}
	}
	return false
}

// annotator adds notes to a private copy of the readings.
type annotator struct {
	rows  []model.LimitReading
	index map[string]int
	u     *Unit
	fsys  fs.FS
}

// Annotate returns a copy of readings with a note added for every directive
// of u that lands on a row, and the same rows otherwise. It never adds a row
// and never changes a Status, Limit or Usage.
//
// A note says what systemd configured ("systemd: TasksMax=7 on a.service"),
// or, when the kernel holds something else, "systemd: configured TasksMax=7
// on a.service, effective 5". cgroup directives are compared with the kernel
// file at the UNIT'S OWN level (u.ControlGroup), read through fsys, not with
// the row's displayed level, which can be an ancestor with a tighter limit. A
// row that is absent, unreadable or unparseable is left alone, and so is a
// directive this systemd did not report. A directive that is reported but not
// understood is noted as such and is never guessed at.
func Annotate(readings []model.LimitReading, u *Unit, fsys fs.FS) []model.LimitReading {
	a := &annotator{rows: make([]model.LimitReading, len(readings)), index: map[string]int{}, u: u, fsys: fsys}
	copy(a.rows, readings)
	for i, r := range a.rows {
		if r.Status == model.StatusUnreadable || r.Status == model.StatusUnparseable {
			continue
		}
		if _, dup := a.index[r.Key]; !dup {
			a.index[r.Key] = i
		}
	}

	a.scalar("TasksMax", "pids.max", u.tasksMax)
	a.scalar("MemoryMax", "memory.max", u.memoryMax)
	a.scalar("MemoryHigh", "memory.high", u.memoryHigh)
	a.cpu()
	for _, l := range rlimitProps {
		if a.rlimitSingle("Limit"+l.name, l.segment) {
			continue
		}
		a.rlimit("Limit"+l.name, l.segment, "hard")
		a.rlimit("Limit"+l.name+"Soft", l.segment, "soft")
	}
	return a.rows
}

// row finds the usable row with key.
func (a *annotator) row(key string) (int, bool) {
	i, ok := a.index[key]
	return i, ok
}

func (a *annotator) note(i int, text string) {
	if a.rows[i].Notes != "" {
		a.rows[i].Notes += "; "
	}
	a.rows[i].Notes += text
}

// matched is the note for a directive the kernel agrees with.
func (a *annotator) matched(i int, what string) {
	a.note(i, fmt.Sprintf("systemd: %s on %s", what, a.u.ID))
}

// differs is the note for a directive the kernel does not hold as configured.
func (a *annotator) differs(i int, what, effective string) {
	a.note(i, fmt.Sprintf("systemd: configured %s on %s, effective %s", what, a.u.ID, effective))
}

func (a *annotator) notUnderstood(i int, prop string, d directive) {
	a.note(i, fmt.Sprintf("systemd: %s=%s on %s was not understood", prop, quoteBounded(d.raw), a.u.ID))
}

func (a *annotator) notCompared(i int, what, file string, err error) {
	a.note(i, fmt.Sprintf("systemd: %s on %s, not compared: %s at the unit level: %s",
		what, a.u.ID, file, sources.Describe(err)))
}

// scalar annotates a cgroup row whose file is a number or "max".
func (a *annotator) scalar(prop, file string, d directive) {
	if d.state == stateAbsent {
		return
	}
	i, ok := a.row("cgroup." + file)
	if !ok {
		return
	}
	if d.state == stateBad {
		a.notUnderstood(i, prop, d)
		return
	}
	what := prop + "=" + d.raw
	fl, err := cgroup.ReadFileLimit(a.fsys, a.u.ControlGroup, file)
	if err != nil {
		a.notCompared(i, what, file, err)
		return
	}
	switch {
	case d.state == stateInfinite && fl.Unlimited,
		d.state == stateFinite && !fl.Unlimited && fl.Value == d.value:
		a.matched(i, what)
	case d.state == stateFinite && !fl.Unlimited && (file == "memory.max" || file == "memory.high") &&
		roundedDown(d.value, fl.Value):
		a.note(i, fmt.Sprintf("systemd: %s on %s, effective %d, consistent with rounding down to whole pages",
			what, a.u.ID, fl.Value))
	case fl.Unlimited:
		a.differs(i, what, "max (not applied)")
	default:
		a.differs(i, what, strconv.FormatUint(fl.Value, 10))
	}
}

// cpu annotates cgroup.cpu.max. The directive is microseconds of CPU per
// second; cpu.max is quota per period, so they are compared exactly as
// directive*period == quota*1_000_000 in 128 bits, with no division.
func (a *annotator) cpu() {
	d := a.u.cpuQuota
	if d.state == stateAbsent {
		return
	}
	i, ok := a.row("cgroup.cpu.max")
	if !ok {
		return
	}
	if d.state == stateBad {
		a.notUnderstood(i, "CPUQuotaPerSecUSec", d)
		return
	}
	what := "CPUQuotaPerSecUSec=" + d.raw
	if p := a.u.cpuPeriod; p != "" && p != infinity {
		if _, ok := parseTimespan(p); ok {
			what += " (period " + p + ")"
		}
	}
	fl, err := cgroup.ReadFileLimit(a.fsys, a.u.ControlGroup, "cpu.max")
	if err != nil {
		a.notCompared(i, what, "cpu.max", err)
		return
	}
	if d.state == stateInfinite {
		if fl.Unlimited {
			a.matched(i, what)
			return
		}
		a.differs(i, what, cpuText(fl))
		return
	}
	if fl.Unlimited {
		a.differs(i, what, "max (not applied)")
		return
	}
	switch {
	case quotaExact(d.value, fl):
		a.matched(i, what)
	case quotaFloored(d.value, fl):
		a.note(i, fmt.Sprintf("systemd: %s on %s, effective %s, consistent with the quota being floored "+
			"to a whole microsecond per period", what, a.u.ID, cpuText(fl)))
	default:
		a.differs(i, what, cpuText(fl))
	}
}

// quotaExact reports whether perSecond microseconds of CPU per second is
// exactly quota per period: perSecond*period == quota*1_000_000, compared in
// 128 bits so neither product can overflow.
func quotaExact(perSecond uint64, fl cgroup.FileLimit) bool {
	lhi, llo := bits.Mul64(perSecond, fl.Period)
	rhi, rlo := bits.Mul64(fl.Value, 1_000_000)
	return lhi == rhi && llo == rlo
}

// quotaFloored reports whether the file's quota is the per-period quota
// floored to a whole microsecond, floor(perSecond*period / 1_000_000), which
// is what systemd writes when the exact figure is fractional (observed:
// CPUQuota=33.33% with a 7ms period gives "2333 7000"). The division is done
// on the 128-bit product; a quotient that does not fit in 64 bits is no match.
func quotaFloored(perSecond uint64, fl cgroup.FileLimit) bool {
	hi, lo := bits.Mul64(perSecond, fl.Period)
	if hi >= 1_000_000 {
		return false
	}
	q, _ := bits.Div64(hi, lo, 1_000_000)
	return q == fl.Value
}

func cpuText(fl cgroup.FileLimit) string {
	return fmt.Sprintf("%dus per %dus", fl.Value, fl.Period)
}

// rlimitMatches reports whether directive d agrees with the row's limit.
func rlimitMatches(d directive, row model.LimitReading) bool {
	return d.state == stateInfinite && !row.Limit.Known ||
		d.state == stateFinite && row.Limit.Known && row.Limit.Value == d.value
}

// rlimitSingle handles a row where soft and hard are equal (a .single row)
// when both directives were reported, are understood, are written the same
// way and both match: one note covers both. It reports whether it did; any
// other case is left to rlimit, so a mismatch is always noted on its own.
func (a *annotator) rlimitSingle(prop, segment string) bool {
	hard, okH := a.u.limits[prop]
	soft, okS := a.u.limits[prop+"Soft"]
	if !okH || !okS || hard.raw != soft.raw || hard.state == stateBad || soft.state == stateBad {
		return false
	}
	if _, split := a.row("rlimit." + segment + ".hard"); split {
		return false
	}
	if _, split := a.row("rlimit." + segment + ".soft"); split {
		return false
	}
	i, ok := a.row("rlimit." + segment + ".single")
	if !ok || !rlimitMatches(hard, a.rows[i]) {
		return false
	}
	a.note(i, fmt.Sprintf("systemd: %s=%s (soft and hard) on %s", prop, hard.raw, a.u.ID))
	return true
}

// rlimit annotates the rlimit row for one Limit<X> property. kind is "hard"
// for Limit<X> and "soft" for Limit<X>Soft; a row where soft and hard are
// equal is a single row and carries both.
func (a *annotator) rlimit(prop, segment, kind string) {
	d, ok := a.u.limits[prop]
	if !ok {
		return
	}
	i, ok := a.row("rlimit." + segment + "." + kind)
	if !ok {
		i, ok = a.row("rlimit." + segment + ".single")
	}
	if !ok {
		return
	}
	if d.state == stateBad {
		a.notUnderstood(i, prop, d)
		return
	}
	row := a.rows[i]
	what := prop + "=" + d.raw
	switch {
	case rlimitMatches(d, row):
		a.matched(i, what)
	case row.Limit.Known:
		a.differs(i, what, strconv.FormatUint(row.Limit.Value, 10))
	default:
		a.differs(i, what, "unlimited")
	}
}
