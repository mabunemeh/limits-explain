// Package rlimits implements the rlimit source: it parses the soft and hard
// resource limits of a process from /proc/<pid>/limits.
package rlimits

import (
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/mabunemeh/limits-explain/internal/model"
	"github.com/mabunemeh/limits-explain/internal/sources"
)

// Name is the value of the Source field on every reading this source emits.
const Name = "rlimit"

const unlimited = "unlimited"

// fieldSplit separates columns: runs of two or more spaces. A limit NAME may
// itself contain single spaces ("Max open files"), so splitting on single
// spaces would shred it. Values (numbers, "unlimited") never contain spaces
// at all.
var fieldSplit = regexp.MustCompile(` {2,}`)

// slugRun matches the characters replaced when slugifying an unknown row
// name into a key segment.
var slugRun = regexp.MustCompile(`[^a-z0-9]+`)

// The kernel formats each row as name.ljust(25) + " " + soft.ljust(20) + " "
// + hard.ljust(20) + " " + unit.ljust(10) (see proc_pid_limits in
// fs/proc/base.c). A value that exactly fills its 20-character field leaves
// no padding, so it is separated from its neighbour by exactly ONE space
// instead of the usual run of two or more. This column drift can happen on
// either side independently:
//
//   - Both sides drift: `prlimit --fsize=18446744073709551614` (20 digits,
//     RLIM_INFINITY - 1) sets soft and hard to values that both fill their
//     field exactly, giving
//     "...size             18446744073709551614 18446744073709551614 bytes".
//     Only ONE run of 2+ spaces survives (before the name), so a 2+-space
//     split of the whole row yields 2 fields.
//   - One side drifts: `prlimit --fsize=10000000000000000000:unlimited`
//     leaves soft exactly 20 characters but hard padded normally, giving
//     "...size             10000000000000000000 unlimited            bytes".
//     The run between soft and hard vanishes but the run before "bytes"
//     survives, so the 2+-space split yields 3 fields, but the first of
//     them ("10000000000000000000 unlimited") is soft and hard glued
//     together rather than a clean pair. parseRow retries on single spaces.
//
// The same collapse can hit the NAME field: a kernel row name that overflows
// its 25-character field leaves a single space before the soft value, which
// is structurally indistinguishable from column drift, so such a row has no
// recoverable name/value boundary and degrades to unparseable. That is by
// design: it fails closed on that one row, never guesses a name, and every
// other row still parses.

// kernelKeyMap maps a real kernel row name to a stable machine key segment,
// using the short names of prlimit(1).
var kernelKeyMap = map[string]string{
	"Max cpu time":          "cpu",
	"Max file size":         "fsize",
	"Max data size":         "data",
	"Max stack size":        "stack",
	"Max core file size":    "core",
	"Max resident set":      "rss",
	"Max processes":         "nproc",
	"Max open files":        "nofile",
	"Max locked memory":     "memlock",
	"Max address space":     "as",
	"Max file locks":        "locks",
	"Max pending signals":   "sigpending",
	"Max msgqueue size":     "msgqueue",
	"Max nice priority":     "nice",
	"Max realtime priority": "rtprio",
	"Max realtime timeout":  "rttime",
}

// Source reads soft and hard rlimits from proc/<pid>/limits.
type Source struct{}

// New returns the rlimit source.
func New() *Source { return &Source{} }

// Name implements [sources.Source].
func (*Source) Name() string { return Name }

// Collect implements [sources.Source]. It reads proc/<pid>/limits from fsys.
func (s *Source) Collect(pid int, fsys fs.FS) ([]model.LimitReading, error) {
	path := fmt.Sprintf("proc/%d/limits", pid)
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		// Covers both "gone" (the process exited between the CLI's
		// directory check and this read, a real race and not a
		// hypothetical one) and "permission denied" (hidepid=2 mounts,
		// another user's PID). Both are structural: there is nothing left
		// in this file to report, so both are errors rather than an empty
		// result. The cause stays wrapped so callers can tell them apart
		// with errors.Is.
		return nil, &sources.SourceError{
			Path:   path,
			Detail: "unreadable: " + sources.Describe(err),
			Err:    err,
		}
	}

	lines := sources.SplitLines(string(data))
	if !strings.HasPrefix(lines[0], "Limit") {
		return nil, &sources.SourceError{
			Path:   path,
			Detail: "missing or invalid header (expected first line to start with 'Limit')",
		}
	}

	dataLines := lines[1:]
	if !anyNonBlank(dataLines) {
		// A real /proc/<pid>/limits always has at least 16 rows. A header
		// with nothing after it is not a process with no limits, it is a
		// truncated or corrupt read.
		return nil, &sources.SourceError{
			Path:   path,
			Detail: "no data rows (header present but file has no limit rows)",
		}
	}

	var readings []model.LimitReading
	// Kept per call: two rows sharing a kernel name (never seen on a real
	// kernel, but a hand-edited or corrupt file could have one) would
	// otherwise collide on the same key and silently shadow each other for
	// any consumer that indexes by key.
	seenKeys := map[string]int{}
	for i, line := range dataLines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lineNo := i + 2 // line 1 is the header; the first data row is line 2
		for _, r := range s.parseRow(lineNo, line) {
			readings = append(readings, dedupeKey(r, seenKeys))
		}
	}
	return readings, nil
}

func anyNonBlank(lines []string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			return true
		}
	}
	return false
}

// humanLabel maps a kernel row name to a lowercase human label:
// "Max open files" becomes "open files". Unknown or new rows go through the
// same rule rather than being dropped, whether or not they start with "Max ".
func humanLabel(kernelName string) string {
	return strings.ToLower(strings.TrimPrefix(kernelName, "Max "))
}

// keySegment maps a kernel row name to a stable machine key segment. Known
// rows use their prlimit(1)-style short name ("nofile"); an unknown or new
// row is slugified from its human label instead of being dropped, so it
// still gets a stable, greppable key.
func keySegment(kernelName string) string {
	if seg, ok := kernelKeyMap[kernelName]; ok {
		return seg
	}
	slug := strings.Trim(slugRun.ReplaceAllString(humanLabel(kernelName), "_"), "_")
	if slug == "" {
		return "unknown"
	}
	return slug
}

// parseValue parses one soft or hard cell. The boolean reports whether the
// cell was valid; an "unlimited" cell is valid and yields an unknown Amount,
// so callers must check the boolean rather than the Amount to tell
// "unlimited" from "failed to parse".
//
// Only "unlimited" or a string of ASCII digits is accepted, so a near-miss
// degrades the whole row to unparseable instead of being silently accepted.
// strconv.ParseUint with base 10 is already this strict on its own: unlike
// ParseInt it permits no sign at all, and it rejects underscores, hex and
// float forms and non-ASCII digits. The explicit digit check is kept as
// defence in depth: it states the intent at the call site and keeps the
// strictness independent of the base argument, which matters because base 0
// would accept "0x400" and "10_240" and would read a leading zero as octal,
// turning "0000050" into 40. A number too large for uint64 also fails; the
// kernel cannot emit one.
func parseValue(raw string) (model.Amount, bool) {
	if raw == unlimited {
		return model.UnknownAmount(), true
	}
	if !sources.IsASCIIDigits(raw) {
		return model.UnknownAmount(), false
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return model.UnknownAmount(), false
	}
	return model.KnownAmount(v), true
}

// tokensToTriple returns (soft, hard, unit) from a token list. The unit
// defaults to "" for two tokens, as some rows ("Max nice priority") have an
// empty units column.
func tokensToTriple(tokens []string) (soft, hard, unit string, ok bool) {
	switch len(tokens) {
	case 3:
		return tokens[0], tokens[1], tokens[2], true
	case 2:
		return tokens[0], tokens[1], "", true
	default:
		return "", "", "", false
	}
}

// splitTailCoarse splits the (soft, hard[, unit]) tail with the normal
// 2+-space column split. It is correct for a normally padded row and for the
// "no unit" case, and wrong when column drift removes a run that would
// normally separate two of the three fields; parseRow then falls back to
// splitTailDrifted.
func splitTailCoarse(tail string) (soft, hard, unit string, ok bool) {
	var tokens []string
	for _, tok := range fieldSplit.Split(strings.TrimSpace(tail), -1) {
		if tok != "" {
			tokens = append(tokens, tok)
		}
	}
	return tokensToTriple(tokens)
}

// splitTailDrifted splits the tail on single spaces. It recovers rows where
// a value fills its fixed-width field exactly and the usual run of spaces
// shrinks to one (see the comment above kernelKeyMap). It is tried only
// after the coarse split fails to produce two values that both parse.
func splitTailDrifted(tail string) (soft, hard, unit string, ok bool) {
	var tokens []string
	for _, tok := range strings.Split(strings.TrimSpace(tail), " ") {
		if tok != "" {
			tokens = append(tokens, tok)
		}
	}
	return tokensToTriple(tokens)
}

// unparseable builds the degraded reading for a row that could not be
// parsed.
//
// This is a per-row failure, not a [sources.SourceError]: one bad row must
// never hide every other reading in the file. The key includes the line
// number because, without a recovered kernel name, nothing else would make
// it distinct from another unparseable row in the same file.
func unparseable(lineNo int, line string) model.LimitReading {
	return model.LimitReading{
		Key:    fmt.Sprintf("rlimit.unparseable.%d", lineNo),
		Kind:   model.KindSingle,
		Label:  "",
		Source: Name,
		Limit:  model.UnknownAmount(),
		Usage:  model.UnknownAmount(),
		Unit:   "",
		Status: model.StatusUnparseable,
		Notes:  fmt.Sprintf("unparseable: %q", line),
	}
}

// dedupeKey suffixes a reading's key with a collision counter if the key was
// already used in this Collect call. The first occurrence keeps its plain
// key; the second gets ".2", the third ".3", and so on.
func dedupeKey(r model.LimitReading, seen map[string]int) model.LimitReading {
	seen[r.Key]++
	if n := seen[r.Key]; n > 1 {
		r.Key = fmt.Sprintf("%s.%d", r.Key, n)
	}
	return r
}

func (s *Source) parseRow(lineNo int, line string) []model.LimitReading {
	if line[0] == ' ' || line[0] == '\t' {
		// A row is never legitimately indented. Splitting "   1024   2048
		// files" on 2+ spaces would yield a leading empty field, which
		// would otherwise be accepted as a real row with an empty name and
		// real-looking numbers. Reject it up front.
		return []model.LimitReading{unparseable(lineNo, line)}
	}

	// Split off only the row NAME here (at most two pieces): the remaining
	// tail (soft, hard, unit) keeps whatever spacing it actually has,
	// single or double, so both split strategies below can be tried against
	// it without re-deriving it.
	nameSplit := fieldSplit.Split(strings.TrimRightFunc(line, unicode.IsSpace), 2)
	if len(nameSplit) != 2 {
		// No run of 2+ spaces anywhere after the first token: there is no
		// recoverable column structure at all (for example a line with
		// only single spaces throughout).
		return []model.LimitReading{unparseable(lineNo, line)}
	}
	kernelName, tail := nameSplit[0], nameSplit[1]

	splitters := []func(string) (string, string, string, bool){splitTailCoarse, splitTailDrifted}
	for _, split := range splitters {
		softRaw, hardRaw, unit, ok := split(tail)
		if !ok {
			continue
		}
		soft, softOK := parseValue(softRaw)
		hard, hardOK := parseValue(hardRaw)
		if !softOK || !hardOK {
			// This split produced tokens but they are not valid values,
			// for example the coarse split gluing a drifted value onto its
			// neighbour ("10000000000000000000 unlimited"). Try the next
			// strategy rather than giving up.
			continue
		}

		label := humanLabel(kernelName)
		segment := keySegment(kernelName)
		if soft == hard {
			return []model.LimitReading{s.reading(segment, model.KindSingle, label, soft, unit)}
		}
		return []model.LimitReading{
			s.reading(segment, model.KindSoft, label, soft, unit),
			s.reading(segment, model.KindHard, label, hard, unit),
		}
	}

	return []model.LimitReading{unparseable(lineNo, line)}
}

func (s *Source) reading(
	segment string, kind model.Kind, label string, limit model.Amount, unit string,
) model.LimitReading {
	return model.LimitReading{
		Key:    fmt.Sprintf("rlimit.%s.%s", segment, kind),
		Kind:   kind,
		Label:  label,
		Source: s.Name(),
		Limit:  limit,
		Usage:  model.UnknownAmount(),
		Unit:   unit,
		Status: model.DefaultStatus(limit, model.UnknownAmount()),
	}
}
