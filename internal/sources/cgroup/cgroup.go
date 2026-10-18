// Package cgroup implements the cgroup v2 source: it reports the memory,
// memory-throttle, task-count and CPU-bandwidth limits that bind a process
// through the control group it lives in.
//
// The source supports the cgroup v2 unified hierarchy only. A host running
// cgroup v1 or the hybrid layout is reported as a structural error that says
// which of the two it found; the tool will not print v1 numbers.
//
// The effective limit of a controller is the MINIMUM across the process's
// own (leaf) cgroup and every ancestor up to the root, because the kernel
// enforces every level independently and the tightest one wins. That
// correlation is what this tool exists for: a process can be capped at 50
// tasks by a slice three levels above it while its own leaf says 100, and
// reading only the leaf would report the wrong ceiling. The notes of every
// reading name the binding level and, when it differs from the leaf, what
// the leaf alone would have allowed.
//
// The unified mount point is taken to be sys/fs/cgroup, the convention on
// every systemd host. A host that mounts cgroup2 somewhere else is out of
// scope for v0.1.0 and gets a clear error rather than wrong numbers.
//
// The cgroup path in proc/<pid>/cgroup is relative to the cgroup NAMESPACE
// of the process doing the reading, so inside a container it may be "/" or a
// shortened path that does not resolve under the host mount. That case is
// reported as a structural error and never guessed at.
//
// Only the four known controller files (memory.max, memory.high, pids.max,
// cpu.max) are ever opened, so an unknown or future controller file cannot
// crash the walk. A known file whose CONTENT is in a form this source does
// not recognise degrades that one reading to unparseable; it never hides the
// other three.
//
// This source reports limits only. Every reading has an unknown Usage, so its
// status is unmeasured, unlimited, unreadable or unparseable, never ok.
package cgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"math/bits"
	"path"
	"strconv"
	"strings"

	"github.com/mabunemeh/limits-explain/internal/model"
	"github.com/mabunemeh/limits-explain/internal/sources"
)

// Name is the value of the Source field on every reading this source emits.
const Name = "cgroup"

// mount is where the unified hierarchy is taken to be mounted, relative to
// the fs.FS root. See the package comment for why this is a fixed convention.
const mount = "sys/fs/cgroup"

// maxRawNote bounds how much of an unparseable file is pasted into a note. A
// corrupt controller file could be arbitrarily large and a note is a single
// table cell.
const maxRawNote = 64

// valueKind classifies what one level said about one controller.
type valueKind int

const (
	// kindSkipped: the controller file does not exist at this level.
	kindSkipped valueKind = iota
	// kindUnlimited: the file said "max", so this level imposes no ceiling.
	kindUnlimited
	// kindFinite: the file carried a number.
	kindFinite
	// kindUnreadable: the file exists but could not be read.
	kindUnreadable
	// kindUnparseable: the file was read but its content is not recognised.
	kindUnparseable
)

// level is one cgroup in the ancestor chain.
type level struct {
	// display is the cgroup path as a human writes it, such as "/a/b".
	display string
	// dir is the slash-separated directory within the fs.FS.
	dir string
}

// parsed is the verdict on one controller file's content.
type parsed struct {
	kind valueKind
	// value is the number reported as the limit. For cpu.max it is the
	// derived millicores, which is truncated toward zero.
	value uint64
	// num and den are the EXACT quantity this level allows, as a fraction,
	// and they are what levels are compared by. A scalar controller sets
	// den to 1 and they are just the value. cpu.max sets them to the raw
	// quota and period, so two levels are compared before the division to
	// millicores rather than after it.
	//
	// Comparing after the division would be wrong, and not only in the last
	// digit: a parent of "1000 1000000" (1.0 millicore) and a leaf of
	// "1500 1000000" (1.5) both truncate to 1, the tie would then go to the
	// leaf, and the tool would name the leaf as binding and quote the
	// leaf's raw pair -- pointing at the wrong cgroup and offering a raw
	// pair 50% higher than the one the kernel enforces. The headline number
	// would still be right (truncation is monotone, so the minimum of the
	// truncated values equals the truncation of the true minimum), but the
	// attribution is the thing this source exists to get right.
	num, den uint64
	// truncated says the division to value lost a remainder, so value is
	// smaller than the real allowance and the raw pair is the exact figure.
	truncated bool
	// detail is the reason for kindUnparseable.
	detail string
}

// lessExact reports whether a allows strictly less than b, comparing the
// fractions exactly by cross-multiplying into 128 bits so neither product
// can overflow.
func lessExact(a, b parsed) bool {
	ahi, alo := bits.Mul64(a.num, b.den)
	bhi, blo := bits.Mul64(b.num, a.den)
	if ahi != bhi {
		return ahi < bhi
	}
	return alo < blo
}

// controller describes one of the four files this source reads.
type controller struct {
	file  string
	label string
	unit  string
	// parse turns the trimmed file content into a verdict.
	parse func(content string) parsed
	// isCPU selects the cpu.max-only note clause carrying the raw pair.
	isCPU bool
	// throttle selects the memory.high-only explanatory clause.
	throttle bool
}

// controllers is the fixed read order, which is also the output order. Keys
// are fixed and unique, so no collision counter is needed here.
var controllers = []controller{
	{file: "memory.max", label: "memory", unit: "bytes", parse: parseScalar},
	{file: "memory.high", label: "memory (high)", unit: "bytes", parse: parseScalar, throttle: true},
	{file: "pids.max", label: "tasks", unit: "processes", parse: parseScalar},
	{file: "cpu.max", label: "cpu bandwidth", unit: "millicores", parse: parseCPU, isCPU: true},
}

// Source reads cgroup v2 limits for a process.
type Source struct{}

// New returns the cgroup source.
func New() *Source { return &Source{} }

// Name implements [sources.Source].
func (*Source) Name() string { return Name }

// Collect implements [sources.Source]. It resolves the cgroup of pid from
// proc/<pid>/cgroup and reads the controller files of that cgroup and of
// every ancestor under sys/fs/cgroup.
func (s *Source) Collect(pid int, fsys fs.FS) ([]model.LimitReading, error) {
	file := fmt.Sprintf("proc/%d/cgroup", pid)
	leafPath, err := resolveCgroup(file, fsys)
	if err != nil {
		return nil, err
	}
	chain, err := buildChain(file, leafPath)
	if err != nil {
		return nil, err
	}
	if err := checkMount(fsys, chain[0]); err != nil {
		return nil, err
	}

	readings := make([]model.LimitReading, 0, len(controllers))
	for _, c := range controllers {
		if r, ok := collectController(fsys, c, chain); ok {
			readings = append(readings, r)
		}
	}
	return readings, nil
}

// resolveCgroup returns the unified cgroup path from the proc cgroup file,
// such as "/a/b".
func resolveCgroup(file string, fsys fs.FS) (string, error) {
	data, err := fs.ReadFile(fsys, file)
	if err != nil {
		// Covers the process exiting mid-read and a permission denial. Both
		// are structural: this file is the only thing that says where the
		// process lives, so there is nothing left to report without it.
		return "", &sources.SourceError{
			Path:   file,
			Detail: "unreadable: " + sources.Describe(err),
			Err:    err,
		}
	}

	var unified []string
	var legacy []string // the controller field of every non-unified entry, in file order
	entries := 0
	for _, line := range sources.SplitLines(string(data)) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// A cgroup path may itself contain a colon, which is exactly why
		// this is SplitN with a limit of 3 and not strings.Split: only the
		// first two colons are separators.
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			// This is also where a cgroup path containing a newline lands:
			// the second half of the path becomes a line of its own with no
			// colons. Failing closed beats resolving a truncated path.
			//
			// A path containing a bare CR takes a different route and is
			// also safe, though less directly: SplitLines only strips a
			// TRAILING CR, so "0::/a\r0::/b" stays one entry whose path is
			// "/a\r0::/b". That is structurally a valid unified entry, so
			// it is accepted here and then fails the leaf-directory check
			// below, because no such directory exists. No real kernel emits
			// a CR, and either way no wrong number escapes.
			return "", &sources.SourceError{
				Path:   file,
				Detail: fmt.Sprintf("malformed entry %q (expected hierarchy-ID:controllers:path)", line),
			}
		}
		entries++
		// Only the unified entry has hierarchy ID 0 and an empty controller
		// list. A v1 hierarchy has a non-zero ID and names its controllers.
		if fields[0] == "0" && fields[1] == "" {
			unified = append(unified, fields[2])
		} else {
			legacy = append(legacy, fmt.Sprintf("%q", fields[1]))
		}
	}

	// The two messages differ on purpose: a reader must be able to tell from
	// the text alone which hierarchy their host has.
	const v2Only = "limits-explain reads the cgroup v2 unified hierarchy only and will not report v1 numbers"
	switch {
	case entries == 0:
		return "", &sources.SourceError{Path: file, Detail: "empty: no cgroup entries"}
	case len(legacy) > 0 && len(unified) > 0:
		return "", &sources.SourceError{Path: file, Detail: fmt.Sprintf(
			"hybrid cgroup hierarchy: a unified entry is present alongside cgroup v1 controllers %s; %s",
			strings.Join(legacy, " "), v2Only)}
	case len(legacy) > 0:
		return "", &sources.SourceError{Path: file, Detail: fmt.Sprintf(
			"cgroup v1 hierarchy: found v1 controllers %s and no unified entry; %s",
			strings.Join(legacy, " "), v2Only)}
	case len(unified) > 1:
		// A real kernel emits one unified line. A corrupt or hand-edited
		// file must not be guessed at.
		return "", &sources.SourceError{
			Path:   file,
			Detail: fmt.Sprintf("%d unified cgroup entries (expected exactly one)", len(unified)),
		}
	}
	if !strings.HasPrefix(unified[0], "/") {
		return "", &sources.SourceError{
			Path:   file,
			Detail: fmt.Sprintf("cgroup path %q is not absolute", unified[0]),
		}
	}
	return unified[0], nil
}

// buildChain returns the leaf cgroup first and the root cgroup last.
//
// For "/a/b c" the result is "/a/b c", "/a", "/". There is no recursion and
// no depth limit beyond the number of path components. file names the proc
// file the path came from, for error reporting.
func buildChain(file, leafPath string) ([]level, error) {
	var components []string
	for _, c := range strings.Split(leafPath, "/") {
		if c == "" {
			continue
		}
		// A real kernel never emits "." or "..", but a hostile or corrupt
		// file can, and walking one would read a directory outside the
		// cgroup tree. It is refused here, before any filesystem access.
		// Every other byte a directory name may hold (spaces, "@", ":",
		// "-", ".") is legal and passes through.
		if c == "." || c == ".." {
			return nil, &sources.SourceError{
				Path: file,
				Detail: fmt.Sprintf("cgroup path %q contains the component %q; "+
					"the path is refused rather than walked", leafPath, c),
			}
		}
		components = append(components, c)
	}

	chain := make([]level, 0, len(components)+1)
	for n := len(components); n >= 0; n-- {
		dir := path.Join(append([]string{mount}, components[:n]...)...)
		// Dropping empty components and refusing "." and ".." above is what
		// makes every built path a valid fs path; fs.FS rejects anything
		// else at read time anyway.
		chain = append(chain, level{display: "/" + strings.Join(components[:n], "/"), dir: dir})
	}
	return chain, nil
}

// checkMount confirms a cgroup v2 hierarchy is mounted and the process's
// own cgroup directory is present under it.
func checkMount(fsys fs.FS, leaf level) error {
	marker := mount + "/cgroup.controllers"
	// cgroup.controllers exists only at the root of a v2 hierarchy, so its
	// presence is the guard that stops a v1 tree mounted at the same place
	// from being read as if it were v2. Its CONTENT is deliberately unused:
	// which controllers are enabled is advisory, and the per-level files
	// are the only truth about a ceiling.
	if _, err := fs.Stat(fsys, marker); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &sources.SourceError{Path: marker, Detail: "the cgroup v2 unified hierarchy is not mounted at " +
				mount + " (this file exists only on cgroup v2), so no v2 limits can be read here", Err: err}
		}
		return &sources.SourceError{Path: marker, Detail: "unreadable: " + sources.Describe(err), Err: err}
	}

	// Without this check the walk would find no controller files anywhere
	// and report "unlimited" for a process that is in fact capped: a wrong
	// answer, which is worse than an error.
	info, err := fs.Stat(fsys, leaf.dir)
	if err == nil && info.IsDir() {
		return nil
	}
	const reason = "the process's cgroup directory is not present under the unified mount " +
		"(likely a container cgroup namespace whose path does not resolve on the host, or the process exited)"
	// Nothing was parsed in either failure, so a stat error is always kept
	// as the cause: callers tell absence from permission with errors.Is.
	// Only "exists but is not a directory" has no cause, because that is a
	// structural surprise rather than a read failure.
	se := &sources.SourceError{Path: leaf.dir, Detail: reason}
	if err != nil {
		se.Err = err
		if !errors.Is(err, fs.ErrNotExist) {
			se.Detail = "unreadable: " + sources.Describe(err) + "; " + reason
		}
	}
	return se
}

// levelResult is what one level said about one controller.
type levelResult struct {
	level  level
	file   string // full path of the controller file at this level
	parsed parsed
	raw    string // trimmed file content, or the read error text for unreadable
}

// collectController reads one controller at every level and aggregates the
// levels into a single reading. The boolean is false when the controller
// file exists at no level, in which case no reading is emitted.
func collectController(fsys fs.FS, c controller, chain []level) (model.LimitReading, bool) {
	results := make([]levelResult, len(chain))
	present := 0
	for i, lv := range chain {
		res := levelResult{level: lv, file: lv.dir + "/" + c.file}
		data, err := fs.ReadFile(fsys, res.file)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Normal: the controller is not enabled at this level, and the
			// root cgroup legitimately lacks some of these files.
			res.parsed = parsed{kind: kindSkipped}
		case err != nil:
			res.parsed = parsed{kind: kindUnreadable}
			res.raw = sources.Describe(err)
			present++
		default:
			res.raw = strings.TrimSpace(string(data))
			res.parsed = c.parse(res.raw)
			present++
		}
		results[i] = res
	}

	// Absence of a controller file at every level is not a ceiling.
	// Claiming "unlimited" would assert knowledge about a controller that is
	// not in this hierarchy at all, so no reading is emitted.
	if present == 0 {
		return model.LimitReading{}, false
	}

	r := model.LimitReading{
		Key:    "cgroup." + c.file,
		Kind:   model.KindSingle,
		Label:  c.label,
		Source: Name,
		Limit:  model.UnknownAmount(),
		Usage:  model.UnknownAmount(),
		Unit:   c.unit,
	}
	var clauses []string
	if c.throttle {
		// This clause comes first and is the reason memory.high is a
		// separate reading at all: presenting a throttle as a kill
		// threshold, or the reverse, is a wrong answer at 3am.
		clauses = append(clauses, "memory.high throttles allocation and forces reclaim; it does NOT kill "+
			"the process, the OOM ceiling is memory.max")
	}
	leaf := chain[0].display

	// The Status ladder, most specific first. A level that could not be
	// parsed outranks one that could not be read because the file was
	// definitely there and definitely wrong, which is a stronger statement
	// than "we could not look". Both outrank any number: the offending
	// level could be the binding one, so reporting the minimum of the
	// levels that did parse could state a ceiling higher than the real one.
	// That wrong number is the one thing this tool must never produce, so
	// the reading fails closed with an unknown limit. Only when every
	// present level is understood do finite values (unmeasured, because no
	// usage is collected yet) outrank a uniform "max" (unlimited): a single
	// finite ceiling anywhere in the chain binds the process no matter how
	// many other levels are unbounded.
	hasUnparseable, hasUnreadable := false, false
	for _, res := range results {
		hasUnparseable = hasUnparseable || res.parsed.kind == kindUnparseable
		hasUnreadable = hasUnreadable || res.parsed.kind == kindUnreadable
	}

	switch {
	case hasUnparseable:
		r.Status = model.StatusUnparseable
		clauses = append(clauses, fmt.Sprintf("leaf %s; limit withheld because a level could not be parsed "+
			"and might be the binding one", leaf))
	case hasUnreadable:
		r.Status = model.StatusUnreadable
		clauses = append(clauses, fmt.Sprintf("leaf %s; limit withheld because a level could not be read "+
			"and might be the binding one", leaf))
	default:
		clauses = append(clauses, aggregateGood(&r, c, results)...)
	}

	if skipped := levelsWhere(results, kindSkipped); len(skipped) > 0 {
		clauses = append(clauses, fmt.Sprintf("%s is absent at %s (normal where the controller is not enabled)",
			c.file, strings.Join(skipped, ", ")))
	}
	for _, res := range results {
		switch res.parsed.kind {
		case kindUnparseable:
			clauses = append(clauses, fmt.Sprintf("unparseable at %s (%s): %s",
				res.level.display, res.file, res.parsed.detail))
		case kindUnreadable:
			clauses = append(clauses, fmt.Sprintf("unreadable at %s (%s): %s",
				res.level.display, res.file, res.raw))
		}
	}
	r.Notes = strings.Join(clauses, "; ")
	return r, true
}

// aggregateGood fills in the limit and status for a controller where every
// present level was readable and parseable, and returns the note clauses.
func aggregateGood(r *model.LimitReading, c controller, results []levelResult) []string {
	leaf := results[0]

	// Scan leafward and keep the FIRST minimum (strict less-than), compared
	// exactly rather than by the reported value. A tie means both levels
	// bind; the leaf-most is reported because it is the cgroup the reader
	// is looking at.
	bind := -1
	for i, res := range results {
		if res.parsed.kind == kindFinite && (bind < 0 || lessExact(res.parsed, results[bind].parsed)) {
			bind = i
		}
	}

	var clauses []string
	if bind >= 0 {
		b := results[bind]
		r.Limit = model.KnownAmount(b.parsed.value)
		r.Status = model.DefaultStatus(r.Limit, r.Usage) // unmeasured until usage is collected
		clauses = append(clauses, fmt.Sprintf("bound by %s (%s)", b.level.display, b.file))
		switch {
		case bind == 0:
			// The binding level is the leaf and was named above.
		case leaf.parsed.kind == kindFinite && c.isCPU:
			// The leaf's own millicores are truncated too, so for cpu the
			// raw pair comes along: without it a leaf of "1500 1000000"
			// reads as "allows 1" next to a binding level of exactly 1,
			// which looks like the leaf imposes the same ceiling when it is
			// in fact looser.
			clauses = append(clauses, fmt.Sprintf("leaf %s allows %d (raw %q)",
				leaf.level.display, leaf.parsed.value, leaf.raw))
		case leaf.parsed.kind == kindFinite:
			clauses = append(clauses, fmt.Sprintf("leaf %s allows %d", leaf.level.display, leaf.parsed.value))
		default:
			clauses = append(clauses, fmt.Sprintf("leaf %s sets no limit of its own", leaf.level.display))
		}
		if c.isCPU {
			clauses = append(clauses, rawCPU(b))
			if b.parsed.truncated {
				// Without this the reader cannot tell 1.0 millicore from
				// 1.9, and a quota below a thousandth of its period would
				// report a flat 0 that reads as "no CPU at all".
				clauses = append(clauses, "millicores are truncated toward zero; "+
					"the raw pair above is the exact limit")
			}
		}
		return clauses
	}

	// Present at one or more levels and every present level said "max".
	r.Status = model.StatusUnlimited
	clauses = append(clauses, fmt.Sprintf("no level imposes a ceiling (leaf %s)", leaf.level.display))
	if c.isCPU {
		for _, res := range results {
			if res.parsed.kind == kindUnlimited {
				clauses = append(clauses, rawCPU(res))
				break
			}
		}
	}
	return clauses
}

// rawCPU quotes the unmodified cpu.max pair of a level.
func rawCPU(res levelResult) string {
	return fmt.Sprintf("raw cpu.max %q (quota/period microseconds) at %s", res.raw, res.level.display)
}

// levelsWhere lists the display names of levels with the given kind.
func levelsWhere(results []levelResult, kind valueKind) []string {
	var names []string
	for _, res := range results {
		if res.parsed.kind == kind {
			names = append(names, res.level.display)
		}
	}
	return names
}

// parseScalar parses memory.max, memory.high and pids.max: "max" or one
// unsigned integer.
func parseScalar(content string) parsed {
	if content == "max" {
		return parsed{kind: kindUnlimited}
	}
	if v, ok := parseUint(content); ok {
		// den 1: a scalar controller's exact allowance is the value itself.
		return parsed{kind: kindFinite, value: v, num: v, den: 1}
	}
	return parsed{kind: kindUnparseable, detail: quoteBounded(content)}
}

// parseUint accepts only a run of ASCII digits that fits in uint64. "max" is
// the only unlimited marker, so 18446744073709551615 is an ordinary finite
// value and survives intact.
func parseUint(s string) (uint64, bool) {
	if !sources.IsASCIIDigits(s) {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	return v, err == nil
}

// parseCPU parses cpu.max, "<quota|max> <period>", into millicores.
func parseCPU(content string) parsed {
	bad := parsed{kind: kindUnparseable, detail: quoteBounded(content)}
	fields := strings.Fields(content)
	if len(fields) != 2 {
		return bad
	}
	period, okP := parseUint(fields[1])
	// A zero period is not kernel output and would divide by zero. The
	// period is validated even when the quota is "max": "unlimited" is a
	// positive claim that no ceiling exists, and a file with a corrupt
	// period is not kernel output, so it must not be allowed to make that
	// claim.
	if !okP || period == 0 {
		return bad
	}
	if fields[0] == "max" {
		return parsed{kind: kindUnlimited}
	}
	quota, okQ := parseUint(fields[0])
	if !okQ {
		return bad
	}
	// 1000 millicores is one full CPU, so "20000 100000" is 200. The
	// product goes through a 128-bit multiply so a quota near 2^63 cannot
	// overflow it. Div64 panics when the quotient does not fit in 64 bits,
	// which happens exactly when hi >= period, so that case is checked
	// first rather than recovered from.
	hi, lo := bits.Mul64(quota, 1000)
	if hi >= period {
		return parsed{kind: kindUnparseable, detail: quoteBounded(content) +
			" (derived millicores do not fit in 64 bits)"}
	}
	millicores, rem := bits.Div64(hi, lo, period)
	// num and den stay the raw pair so levels are compared exactly; see the
	// comment on parsed.
	return parsed{
		kind:      kindFinite,
		value:     millicores,
		num:       quota,
		den:       period,
		truncated: rem != 0,
	}
}

// quoteBounded quotes at most maxRawNote bytes of raw file content, so a
// corrupt file never pastes an unbounded blob into a note.
func quoteBounded(s string) string {
	if len(s) <= maxRawNote {
		return strconv.Quote(s)
	}
	return strconv.Quote(s[:maxRawNote]) + "..."
}
