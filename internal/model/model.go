// Package model holds the data shared by every source and renderer: one
// reading of one limit from one source.
package model

import "strconv"

// Kind says which half of a limit a reading represents.
type Kind string

const (
	// KindSoft is the soft (enforced) half of a limit.
	KindSoft Kind = "soft"
	// KindHard is the hard (ceiling for the soft value) half of a limit.
	KindHard Kind = "hard"
	// KindSingle is used when a source has only one value to report, e.g.
	// soft == hard, so there is nothing to disambiguate.
	KindSingle Kind = "single"
)

// Status is the health of one reading, independent of the Limit and Usage
// values themselves. [DefaultStatus] derives it from them for the common
// case, but a source may set it explicitly (unparseable, unreadable) when
// the failure cannot be expressed as "limit is unknown".
//
// When more than one status could apply, the most specific wins:
//
//  1. [StatusUnparseable]: the row or entry itself could not be parsed. The
//     limit is unknown here too, but because parsing failed, not because
//     the kernel said "no ceiling". This status is what tells the two
//     apart.
//  2. [StatusUnreadable]: reserved for a source where one reading out of
//     many is unreadable (for example permission denied on a single cgroup
//     controller file) while the rest of the source succeeds. The rlimits
//     source never uses it: a whole-file read failure there is a
//     SourceError, because there is nothing left to report once the one
//     file the source reads is gone. The CLI also uses it for a whole source
//     that failed with a read error, surfaced as one degraded row so the
//     other sources' readings are not lost.
//  3. [StatusUnlimited]: the limit is unknown by design, because the kernel
//     reported no ceiling. This outranks unmeasured: with no ceiling there
//     is nothing to measure usage against, and calling an unlimited reading
//     "unmeasured" would imply a ceiling exists and has just not been
//     checked yet.
//  4. [StatusUnmeasured]: the limit is known but usage has not been
//     collected. Every finite-limit reading is in this state until usage
//     collection exists.
//  5. [StatusOK]: both limit and usage are known.
type Status string

const (
	// StatusOK means both limit and usage are known.
	StatusOK Status = "ok"
	// StatusUnlimited means the kernel reported no ceiling.
	StatusUnlimited Status = "unlimited"
	// StatusUnreadable means this reading's input could not be read.
	StatusUnreadable Status = "unreadable"
	// StatusUnmeasured means the limit is known but usage is not.
	StatusUnmeasured Status = "unmeasured"
	// StatusUnparseable means the input was read but could not be parsed.
	StatusUnparseable Status = "unparseable"
)

// Amount is an unsigned quantity that may be absent. It replaces a pointer
// (*uint64) on purpose: in a tool whose entire premise is "this value may be
// unknown", a nil pointer is one forgotten check away from a crash, while
// the zero Amount is a perfectly safe "unknown".
//
// Values are uint64 because the kernel reports rlimits as unsigned 64-bit
// numbers; RLIM_INFINITY is 2^64-1 and a finite limit may legitimately be
// 2^64-2, which overflows int64. Never narrow Value to int or int64.
type Amount struct {
	Value uint64
	Known bool
}

// KnownAmount returns an Amount holding v.
func KnownAmount(v uint64) Amount { return Amount{Value: v, Known: true} }

// UnknownAmount returns an Amount with no value. It is the zero Amount.
func UnknownAmount() Amount { return Amount{} }

// MarshalJSON emits the bare number when the amount is known and null when
// it is not. The number is written as an exact decimal integer, so values
// above 2^53 survive.
func (a Amount) MarshalJSON() ([]byte, error) {
	if !a.Known {
		return []byte("null"), nil
	}
	return strconv.AppendUint(nil, a.Value, 10), nil
}

// LimitReading is a single limit binding a process, as reported by one
// source.
//
// Display strings such as "open files (soft)" are deliberately NOT stored
// here. They are built by the reporting package from Label and Kind, so that
// later stages (ranking, deduplication across sources) work with the
// structured identity and never have to reverse-parse a display string.
type LimitReading struct {
	// Key is a stable machine key such as "rlimit.nofile.soft", built from
	// the kernel row plus Kind. It is meant for cross-source deduplication
	// and ranking and is never shown to a human as-is.
	//
	// It is unique within one source's output for one Collect call (a
	// source suffixes a collision, for example ".2"), but NOT guaranteed
	// unique across sources.
	Key string `json:"key"`
	// Kind says whether this is the soft half, the hard half, or a single
	// value.
	Kind Kind `json:"kind"`
	// Label is the human base name with no soft/hard suffix, such as
	// "open files".
	Label string `json:"label"`
	// Source names the source that produced the reading, such as "rlimit".
	Source string `json:"source"`
	// Limit is the ceiling. It is unknown when the limit is unlimited or,
	// with StatusUnparseable, when parsing failed; Status tells the two
	// apart.
	Limit Amount `json:"limit"`
	// Usage is current usage. It is unknown until usage collection exists,
	// so no reading carries a known Usage yet.
	Usage Amount `json:"usage"`
	// Unit is "files", "bytes", "seconds", "processes" and so on, or empty
	// when the source has no unit for this row.
	Unit string `json:"unit"`
	// Status is the health of the reading; see [Status].
	Status Status `json:"status"`
	// Notes carries free-form detail, such as the offending line of an
	// unparseable row.
	Notes string `json:"notes"`
}

// DefaultStatus derives the status implied by limit and usage alone.
//
// It covers only the "everything parsed fine" cases (unlimited, unmeasured,
// ok), following the precedence documented on [Status]. A source must set
// the status explicitly for failure states such as unparseable or
// unreadable: a failed parse also leaves the limit unknown, which is
// indistinguishable from "unlimited" without an explicit override.
func DefaultStatus(limit, usage Amount) Status {
	if !limit.Known {
		return StatusUnlimited
	}
	if !usage.Known {
		return StatusUnmeasured
	}
	return StatusOK
}

// ConsumedFraction reports how much of Limit is already consumed by Usage.
// 1.0 means fully at the ceiling; the result can exceed 1.0.
//
// The boolean is false when either Limit or Usage is unknown (unlimited,
// unmeasured or unparseable). A Limit of zero is treated as fully consumed
// (1.0) regardless of Usage, rather than dividing by zero.
//
// This is the quantity ranking sorts by: readings are ordered by which
// ceiling is nearest, with unknown fractions sinking to the bottom. The
// result is a float64, which loses precision above 2^53; it is a ranking
// fraction, not an accounting value.
func (r LimitReading) ConsumedFraction() (float64, bool) {
	if !r.Limit.Known || !r.Usage.Known {
		return 0, false
	}
	if r.Limit.Value == 0 {
		return 1.0, true
	}
	return float64(r.Usage.Value) / float64(r.Limit.Value), true
}
