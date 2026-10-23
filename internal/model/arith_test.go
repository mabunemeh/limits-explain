package model

import (
	"math"
	"testing"
)

func known(limit, usage uint64) LimitReading {
	return reading(KnownAmount(limit), KnownAmount(usage))
}

func TestHeadroom(t *testing.T) {
	tests := []struct {
		name      string
		r         LimitReading
		remaining uint64
		overBy    uint64
		ok        bool
	}{
		{"under", known(1024, 512), 512, 0, true},
		{"exactly at the limit", known(1024, 1024), 0, 0, true},
		{"over: no wraparound", known(1000, 1125), 0, 125, true},
		{"zero limit, zero usage", known(0, 0), 0, 0, true},
		{"zero limit, some usage", known(0, 5), 0, 5, true},
		{"usage max, limit one", known(1, math.MaxUint64), 0, math.MaxUint64 - 1, true},
		{"limit max, usage zero", known(math.MaxUint64, 0), math.MaxUint64, 0, true},
		{"limit unknown", reading(UnknownAmount(), KnownAmount(5)), 0, 0, false},
		{"usage unknown", reading(KnownAmount(5), UnknownAmount()), 0, 0, false},
		{"both unknown", reading(UnknownAmount(), UnknownAmount()), 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rem, over, ok := tc.r.Headroom()
			if rem != tc.remaining || over != tc.overBy || ok != tc.ok {
				t.Errorf("Headroom() = (%d, %d, %v), want (%d, %d, %v)",
					rem, over, ok, tc.remaining, tc.overBy, tc.ok)
			}
		})
	}
}

func TestAtOrAbove(t *testing.T) {
	const pow60 = uint64(1) << 60
	tests := []struct {
		name    string
		r       LimitReading
		percent uint64
		want    bool
	}{
		{"exactly 90 percent", known(10, 9), 90, true},
		{"just under 90 percent", known(10000000, 8999999), 90, false},
		{"exactly the threshold at 100", known(10, 10), 100, true},
		{"over 100 percent", known(10, 11), 100, true},
		{"below", known(100, 10), 90, false},
		{"threshold 1", known(1000, 10), 1, true},
		{"threshold 1, just under", known(1000, 9), 1, false},
		// float64 cannot tell 2^60 from 2^60+1, so usage = limit-1 looks like
		// 100% there. Exactly it is just under 100.
		{"2^60: usage one short of limit, threshold 100", known(pow60+1, pow60), 100, false},
		{"2^60: usage at limit, threshold 100", known(pow60+1, pow60+1), 100, true},
		// 90% of 10*2^60-ish: usage*100 overflows 64 bits, the 128-bit
		// compare must still be exact on both sides of the boundary.
		{"huge: exactly 90 percent", known(10*pow60, 9*pow60), 90, true},
		{"huge: one short of 90 percent", known(10*pow60, 9*pow60-1), 90, false},
		{"near 2^64: at limit", known(math.MaxUint64, math.MaxUint64), 100, true},
		{"near 2^64: one short of limit", known(math.MaxUint64, math.MaxUint64-1), 100, false},
		{"near 2^64: one short passes 99", known(math.MaxUint64, math.MaxUint64-1), 99, true},
		{"usage max, limit one", known(1, math.MaxUint64), 100, true},
		{"zero limit, zero usage", known(0, 0), 100, true},
		{"zero limit, zero usage, threshold 1", known(0, 0), 1, true},
		{"zero limit, some usage", known(0, 5), 90, true},
		{"unmeasured: usage unknown", reading(KnownAmount(10), UnknownAmount()), 1, false},
		{"unmeasured: limit unknown", reading(UnknownAmount(), KnownAmount(10)), 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.AtOrAbove(tc.percent); got != tc.want {
				t.Errorf("AtOrAbove(%d) = %v, want %v", tc.percent, got, tc.want)
			}
		})
	}
}

func TestFloat64WouldMisjudgeTheThresholdHere(t *testing.T) {
	// Guard for the test above: confirm the premise, that float64 really
	// does get these wrong, so the exact tests are not passing vacuously.
	const pow60 = uint64(1) << 60
	if float64(pow60) != float64(pow60+1) {
		t.Fatal("expected float64 to tie 2^60 and 2^60+1")
	}
	if float64(pow60)/float64(pow60+1) < 1.0 {
		t.Fatal("expected the float64 fraction to round up to 1.0")
	}
}

func TestCompareConsumed(t *testing.T) {
	const pow60 = uint64(1) << 60
	tests := []struct {
		name string
		a, b LimitReading
		want int
	}{
		{"greater", known(100, 50), known(100, 40), 1},
		{"less", known(100, 40), known(100, 50), -1},
		{"equal fractions, different scale", known(100, 50), known(1000, 500), 0},
		{"over beats exactly full", known(100, 112), known(100, 100), 1},
		// float64 ties these two (both round to 1.0); exactly, the first is
		// one part in 2^60 short.
		{"2^60 vs 2^60+1 differ by one", known(pow60+1, pow60), known(pow60, pow60), -1},
		{"2^60 vs 2^60+1 reversed", known(pow60, pow60), known(pow60+1, pow60), 1},
		{"same limit usage differs by one", known(pow60+1, pow60), known(pow60+1, pow60-1), 1},
		{"near 2^64 equal", known(math.MaxUint64, math.MaxUint64), known(math.MaxUint64, math.MaxUint64), 0},
		{"near 2^64 one apart", known(math.MaxUint64, math.MaxUint64-1), known(math.MaxUint64, math.MaxUint64), -1},
		// A zero limit is fraction 1 at any usage, equal to a full reading.
		{"zero limit equals full", known(0, 0), known(100, 100), 0},
		{"zero limit with usage equals full", known(0, 7), known(100, 100), 0},
		{"zero limit beats partial", known(0, 0), known(100, 99), 1},
		{"over beats zero limit", known(100, 101), known(0, 3), 1},
		{"unmeasured side compares equal", reading(KnownAmount(10), UnknownAmount()), known(10, 5), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CompareConsumed(tc.a, tc.b); got != tc.want {
				t.Errorf("CompareConsumed = %d, want %d", got, tc.want)
			}
			if tc.want != 0 {
				if got := CompareConsumed(tc.b, tc.a); got != -tc.want {
					t.Errorf("reversed CompareConsumed = %d, want %d", got, -tc.want)
				}
			}
		})
	}
}

func TestPercent(t *testing.T) {
	tests := []struct {
		name string
		r    LimitReading
		want string
		ok   bool
	}{
		{"62 percent", known(1000, 620), "62.0", true},
		{"zero", known(1000, 0), "0.0", true},
		{"half a tenth is floored", known(10000, 1), "0.0", true},
		{"one tenth", known(10000, 10), "0.1", true},
		{"exactly 90", known(10, 9), "90.0", true},
		// Floored, not rounded: 89.99999% must not display as 90.0 or the
		// display would be ahead of --warn 90.
		{"just under 90 floors", known(10000000, 8999999), "89.9", true},
		{"88.96 floors to 88.9", known(10000, 8896), "88.9", true},
		{"full", known(100, 100), "100.0", true},
		{"over", known(1000, 1125), "112.5", true},
		{"zero limit zero usage", known(0, 0), "100.0", true},
		{"zero limit some usage", known(0, 5), "100.0", true},
		{"one third floors", known(3, 1), "33.3", true},
		{"two thirds floors", known(3, 2), "66.6", true},
		{"2^60 limit, one short", known(1<<60+1, 1<<60), "99.9", true},
		// The permille does not fit in 64 bits here.
		{"usage max, limit one", known(1, math.MaxUint64), "1844674407370955161500.0", true},
		{"both max", known(math.MaxUint64, math.MaxUint64), "100.0", true},
		{"unmeasured", reading(KnownAmount(10), UnknownAmount()), "", false},
		{"unlimited", reading(UnknownAmount(), KnownAmount(10)), "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.r.Percent()
			if got != tc.want || ok != tc.ok {
				t.Errorf("Percent() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestMeasured(t *testing.T) {
	if !known(1, 1).Measured() {
		t.Error("both known should be measured")
	}
	if reading(KnownAmount(1), UnknownAmount()).Measured() || reading(UnknownAmount(), KnownAmount(1)).Measured() {
		t.Error("a missing side must not be measured")
	}
}
