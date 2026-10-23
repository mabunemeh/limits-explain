// Package model holds the data shared by every source and renderer: one
// reading of one limit from one source.
package model

import (
	"math/big"
	"math/bits"
	"strconv"
)

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
//  4. [StatusUnmeasured]: the limit is known but usage is not, either
//     because the source has no usage counter for it (cpu bandwidth, the
//     rlimits) or because the counter could not be read or parsed.
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
	// Usage is current usage. It is unknown when the source has no usage
	// counter for this limit or could not read it; Status says which. It is
	// never guessed.
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

// Measured reports whether both Limit and Usage are known, which is what
// every calculation below requires.
func (r LimitReading) Measured() bool { return r.Limit.Known && r.Usage.Known }

// Headroom reports how much room is left under the limit, and by how much
// usage has gone past it.
//
// ok is false when Limit or Usage is unknown. Usage CAN exceed the limit (a
// cgroup limit lowered below current usage, a zero ceiling), so the
// subtraction is only done after the comparison: a bare limit-usage would
// wrap around to a number near 2^64 and report an exhausted resource as
// almost entirely free. At or over the limit, remaining is 0 and overBy is
// the excess (also 0 at exactly the limit).
func (r LimitReading) Headroom() (remaining, overBy uint64, ok bool) {
	if !r.Measured() {
		return 0, 0, false
	}
	if r.Usage.Value >= r.Limit.Value {
		return 0, r.Usage.Value - r.Limit.Value, true
	}
	return r.Limit.Value - r.Usage.Value, 0, true
}

// AtOrAbove reports whether usage has reached percent of the limit, that is
// usage*100 >= percent*limit. It is false for an unmeasured reading.
//
// Both products are formed in 128 bits and compared exactly, with no float64
// anywhere: above 2^53 a float cannot tell limits 2^60 and 2^60+1 apart, so
// a threshold decided in floating point can misjudge a reading at the
// boundary. A zero limit is fully consumed at any usage, including zero, so
// it is at or above every threshold; the right-hand side is 0 and the
// comparison holds without a special case.
func (r LimitReading) AtOrAbove(percent uint64) bool {
	if !r.Measured() {
		return false
	}
	uhi, ulo := bits.Mul64(r.Usage.Value, 100)
	lhi, llo := bits.Mul64(percent, r.Limit.Value)
	if uhi != lhi {
		return uhi > lhi
	}
	return ulo >= llo
}

// fraction returns the consumed fraction of a measured reading as an exact
// num/den pair. A zero limit is normalised to 1/1: fully consumed whatever
// the usage, which also keeps the denominator non-zero.
func (r LimitReading) fraction() (num, den uint64) {
	if r.Limit.Value == 0 {
		return 1, 1
	}
	return r.Usage.Value, r.Limit.Value
}

// CompareConsumed orders two measured readings by consumed fraction: it
// returns -1, 0 or +1 as a is less than, equal to or greater than b. The
// fractions u1/l1 and u2/l2 are compared by cross-multiplying (u1*l2 against
// u2*l1) in 128 bits, so it is exact for any uint64 inputs. A zero limit
// counts as fraction 1, as in [LimitReading.AtOrAbove]. It returns 0 if
// either reading is not measured; callers separate those out first.
func CompareConsumed(a, b LimitReading) int {
	if !a.Measured() || !b.Measured() {
		return 0
	}
	an, ad := a.fraction()
	bn, bd := b.fraction()
	ahi, alo := bits.Mul64(an, bd)
	bhi, blo := bits.Mul64(bn, ad)
	switch {
	case ahi != bhi:
		if ahi < bhi {
			return -1
		}
		return 1
	case alo < blo:
		return -1
	case alo > blo:
		return 1
	}
	return 0
}

// Percent returns the consumed percentage as a decimal string with one digit
// after the point, such as "62.0" or "112.5". The boolean is false for an
// unmeasured reading.
//
// The value is FLOORED to a tenth of a percent, not rounded, and computed
// exactly as floor(usage*1000/limit). Rounding would show "90.0" for a
// reading at 89.96% that did not trip a --warn 90 check; flooring keeps the
// display from ever being ahead of the decision. A zero limit shows
// "100.0". Over 100% is shown as is. math/big is used because the permille
// can exceed 64 bits (usage 2^64-1 against a limit of 1).
func (r LimitReading) Percent() (string, bool) {
	if !r.Measured() {
		return "", false
	}
	num, den := r.fraction()
	permille := new(big.Int).SetUint64(num)
	permille.Mul(permille, big.NewInt(1000))
	permille.Quo(permille, new(big.Int).SetUint64(den))
	whole, tenth := new(big.Int).QuoRem(permille, big.NewInt(10), new(big.Int))
	return whole.String() + "." + tenth.String(), true
}
