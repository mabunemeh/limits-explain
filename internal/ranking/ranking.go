// Package ranking orders readings by which ceiling is nearest and summarises
// the result for the headline and the exit code.
//
// Nothing here goes through float64: ordering and thresholds use the exact
// 128-bit comparisons on [model.LimitReading].
package ranking

import (
	"sort"

	"github.com/mabunemeh/limits-explain/internal/model"
)

// group is the coarse tier a reading sorts into before fractions matter.
type group int

const (
	// groupMeasured: Limit and Usage are both known, so there is a fraction.
	groupMeasured group = iota
	// groupGap: not unlimited, yet no fraction exists (unmeasured,
	// unreadable, unparseable, source_error rows). These sit directly under
	// the measured rows so the gap is seen before the unlimited rows, which
	// are the least interesting thing in the report.
	groupGap
	// groupUnlimited: the kernel reported no ceiling.
	groupUnlimited
)

func groupOf(r model.LimitReading) group {
	switch {
	case r.Measured():
		return groupMeasured
	case r.Status == model.StatusUnlimited:
		return groupUnlimited
	default:
		return groupGap
	}
}

// Rank returns a copy of in ordered nearest ceiling first: measured readings
// by consumed fraction descending, then the readings that could not be
// measured, then the unlimited ones.
//
// The sort is stable, so readings that tie keep their collection order
// (registry order, then source order) and the output is deterministic.
func Rank(in []model.LimitReading) []model.LimitReading {
	out := make([]model.LimitReading, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		gi, gj := groupOf(out[i]), groupOf(out[j])
		if gi != gj {
			return gi < gj
		}
		if gi == groupMeasured {
			return model.CompareConsumed(out[i], out[j]) > 0
		}
		return false
	})
	return out
}

// Summary is what the headline and the exit code need to know about a
// report.
type Summary struct {
	// Total is the number of readings.
	Total int
	// Nearest is the measured reading closest to its ceiling (the first one
	// on a tie). It is only meaningful when HasNearest is true.
	Nearest    model.LimitReading
	HasNearest bool
	// NotMeasured counts readings whose status is neither ok nor unlimited:
	// the gaps, which forbid reading the report as an all-clear.
	NotMeasured int
	// Warned counts measured readings at or above the warn threshold.
	Warned int
}

// Summarize computes the Summary of readings for a warn threshold given as a
// percentage. It does not rely on the input being ranked.
func Summarize(readings []model.LimitReading, warn uint64) Summary {
	s := Summary{Total: len(readings)}
	for _, r := range readings {
		if r.Status != model.StatusOK && r.Status != model.StatusUnlimited {
			s.NotMeasured++
		}
		if !r.Measured() {
			continue
		}
		// Strictly greater: on a tie the earlier reading stays the nearest.
		if !s.HasNearest || model.CompareConsumed(r, s.Nearest) > 0 {
			s.Nearest, s.HasNearest = r, true
		}
		if r.AtOrAbove(warn) {
			s.Warned++
		}
	}
	return s
}
