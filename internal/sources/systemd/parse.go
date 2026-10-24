package systemd

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/mabunemeh/limits-explain/internal/sources"
)

// maxRaw bounds how much systemd output is pasted into an error or a note.
const maxRaw = 64

func quoteBounded(s string) string {
	if len(s) <= maxRaw {
		return strconv.Quote(s)
	}
	return strconv.Quote(s[:maxRaw]) + "..."
}

// numState says how one directive value was understood.
type numState int

const (
	// stateAbsent: this systemd did not report the property. The zero value,
	// so a missing directive is skipped silently.
	stateAbsent numState = iota
	// stateFinite: a number (or, for CPUQuotaPerSecUSec, exact microseconds).
	stateFinite
	// stateInfinite: "infinity".
	stateInfinite
	// stateBad: reported but in a form this parser does not recognise.
	stateBad
)

// directive is one parsed property value. raw is exactly what systemctl
// printed and is what notes quote, so a note never shows a reformatted number.
type directive struct {
	state numState
	value uint64
	raw   string
}

const infinity = "infinity"

// parseNumber understands a count or a byte size: "infinity" or a run of ASCII
// digits that fits in uint64. Anything else is stateBad; it is never guessed.
func parseNumber(raw string) directive {
	d := directive{raw: raw, state: stateBad}
	if raw == infinity {
		d.state = stateInfinite
		return d
	}
	if !sources.IsASCIIDigits(raw) {
		return d
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return d
	}
	d.state, d.value = stateFinite, v
	return d
}

// parseQuota understands CPUQuotaPerSecUSec: "infinity" or a timespan.
func parseQuota(raw string) directive {
	if raw == infinity {
		return directive{state: stateInfinite, raw: raw}
	}
	if us, ok := parseTimespan(raw); ok {
		return directive{state: stateFinite, value: us, raw: raw}
	}
	return directive{state: stateBad, raw: raw}
}

// timespanUnits are the units this parser accepts, in suffix-match order
// ("min" and "ms" must be tried before "s"), with microseconds per unit.
//
// Days, weeks, months and years ("d", "w", "month", "y") and a bare "0" are
// deliberately NOT supported: systemd can print them, but a CPU quota that
// large, or zero, is not something this tool will guess about, so such a value
// fails closed as "not understood" rather than being summed or assumed.
var timespanUnits = []struct {
	suffix string
	micros int64
}{
	{"min", 60_000_000},
	{"ms", 1_000},
	{"us", 1},
	{"h", 3_600_000_000},
	{"s", 1_000_000},
}

// maxTimespanDigits keeps a hostile value from making the big-integer
// arithmetic expensive.
const maxTimespanDigits = 40

// parseTimespan sums space-separated tokens of the form
// <digits>[.<digits>]<unit>, unit one of us, ms, s, min, h, into an exact
// number of microseconds. A token whose fraction does not land on a whole
// microsecond, a sum that overflows uint64, and every other form (a sign, an
// exponent, a missing unit, a space between number and unit, an empty token)
// is rejected rather than rounded.
func parseTimespan(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	total := new(big.Int)
	for _, tok := range strings.Split(s, " ") {
		us, ok := parseTimespanToken(tok)
		if !ok {
			return 0, false
		}
		total.Add(total, us)
	}
	if !total.IsUint64() {
		return 0, false
	}
	return total.Uint64(), true
}

func parseTimespanToken(tok string) (*big.Int, bool) {
	var mult int64
	num := ""
	for _, u := range timespanUnits {
		if strings.HasSuffix(tok, u.suffix) {
			mult, num = u.micros, strings.TrimSuffix(tok, u.suffix)
			break
		}
	}
	if mult == 0 {
		return nil, false
	}
	whole, frac, hasFrac := strings.Cut(num, ".")
	if !sources.IsASCIIDigits(whole) || len(whole) > maxTimespanDigits {
		return nil, false
	}
	if hasFrac && (!sources.IsASCIIDigits(frac) || len(frac) > maxTimespanDigits) {
		return nil, false
	}
	// value = digits(whole+frac) * mult / 10^len(frac), which must divide
	// exactly: a fraction finer than one microsecond is not a whole number of
	// microseconds and is refused.
	digits, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok {
		return nil, false
	}
	digits.Mul(digits, big.NewInt(mult))
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(frac))), nil)
	q, r := new(big.Int).QuoRem(digits, scale, new(big.Int))
	if r.Sign() != 0 {
		return nil, false
	}
	return q, true
}

// parseShow splits `systemctl show` output into properties, failing closed on
// structure: every line must be Key=Value with a non-empty key, and a key
// must not repeat. Only the empty piece after the final newline is allowed.
func parseShow(out string) (map[string]string, error) {
	lines := sources.SplitLines(out)
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	props := make(map[string]string, len(lines))
	for i, line := range lines {
		key, val, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("line %d is not Key=Value: %s", i+1, quoteBounded(line))
		}
		if _, dup := props[key]; dup {
			return nil, fmt.Errorf("line %d repeats the key %s", i+1, quoteBounded(key))
		}
		props[key] = val
	}
	return props, nil
}
