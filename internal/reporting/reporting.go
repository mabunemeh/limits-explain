// Package reporting renders limit readings as a text table or as JSON.
package reporting

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/bits"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"
	"unicode/utf8"

	"github.com/mabunemeh/limits-explain/internal/model"
	"github.com/mabunemeh/limits-explain/internal/ranking"
	"github.com/mabunemeh/limits-explain/internal/sources/systemd"
)

// notAvailable is shown wherever a value is unknown.
const notAvailable = "n/a"

// placeholder is what a control character in a dynamic cell is replaced
// with: visibly wrong, but inert.
const placeholder = '\ufffd'

// DisplayName builds the human display name from a reading Label and Kind,
// for example "open files (soft)".
//
// It is kept out of the data model on purpose: later stages (ranking,
// deduplication across sources) key off Key and Kind directly rather than
// reverse-parsing a display string.
func DisplayName(r model.LimitReading) string {
	label := r.Label
	if label == "" {
		label = "(unparseable row)"
	}
	if r.Kind == model.KindSingle {
		return label
	}
	return fmt.Sprintf("%s (%s)", label, r.Kind)
}

// cell renders one dynamic table cell, neutralising anything that could
// break the layout or the terminal.
//
// Every dynamic cell must go through this. Limit NAMES and units are
// kernel-derived (or, for a hand-edited file, attacker-controlled) strings.
// text/tabwriter lays out columns by tab and newline, so a tab, newline or
// carriage return inside a cell silently destroys the whole table, and a
// control character such as ESC can rewrite the terminal. Control
// characters, Unicode line and paragraph separators and format characters
// (which include the bidirectional overrides that reorder displayed text)
// are replaced with U+FFFD. Invalid UTF-8 comes out as U+FFFD as well.
//
// Unlike a markup-based renderer there is no escaping to do for text such
// as "[/red]": it is printed literally.
func cell(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsControl(r), unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp):
			b.WriteRune(placeholder)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// unitBytes is the Unit value whose amounts the table humanizes.
const unitBytes = "bytes"

// iecUnits names the binary prefixes above plain bytes, in 1024 steps.
var iecUnits = [...]string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}

// humanBytes renders v for the table: "N B" under 1024, otherwise the largest
// IEC unit that keeps the number at 1 or more, with one decimal.
//
// The decimal is FLOORED with exact integer math, never rounded, so a value
// just under its limit can never print equal to the limit: the largest uint64
// is "15.9 EiB", not "16.0 EiB". There is no float64 in here; it would lose
// bits above 2^53.
func humanBytes(v uint64) string {
	if v < 1024 {
		return strconv.FormatUint(v, 10) + " B"
	}
	// bits.Len64(v)-1 is the index of the top set bit and each unit is 10
	// bits, so for v >= 1024 the shift is 10, 20, ... 60.
	shift := uint((bits.Len64(v) - 1) / 10 * 10)
	whole := v >> shift
	rem := v & (1<<shift - 1)
	// rem < 2^60, so rem*10 < 2^64: no overflow.
	tenths := rem * 10 >> shift
	return fmt.Sprintf("%d.%d %s", whole, tenths, iecUnits[shift/10-1])
}

// valueCell renders an exact value, humanized when the unit is bytes.
func valueCell(v uint64, unit string) string {
	if unit == unitBytes {
		return humanBytes(v)
	}
	return strconv.FormatUint(v, 10)
}

// quantityCell renders a known-or-unknown amount of the given unit.
func quantityCell(a model.Amount, unit string) string {
	if !a.Known {
		return notAvailable
	}
	return valueCell(a.Value, unit)
}

// limitCell renders the Limit column, distinguishing "no ceiling" from
// "unknown".
//
// An unknown limit means two different things depending on Status. The
// kernel may have reported no ceiling (StatusUnlimited), which renders as
// "unlimited". Or the row may have failed to parse or read
// (StatusUnparseable, StatusUnreadable), in which case the limit is
// genuinely UNKNOWN and must render as "n/a" like every other unknown
// value. Asserting "unlimited" there would state a fact about a ceiling that
// was never observed.
func limitCell(r model.LimitReading) string {
	if r.Limit.Known {
		return quantityCell(r.Limit, r.Unit)
	}
	if r.Status == model.StatusUnlimited {
		return string(model.StatusUnlimited)
	}
	return notAvailable
}

// headroomCell renders the Headroom column: n/a when unknown, the remaining
// amount, or "over by N" when usage is past the limit. Exactly at the limit
// is "0" (or "0 B" for bytes), not "over by 0".
func headroomCell(r model.LimitReading) string {
	remaining, overBy, ok := r.Headroom()
	switch {
	case !ok:
		return notAvailable
	case overBy > 0:
		return "over by " + valueCell(overBy, r.Unit)
	}
	return valueCell(remaining, r.Unit)
}

// usedCell renders the Used column, the floored percentage of the limit
// consumed, or n/a.
func usedCell(r model.LimitReading) string {
	p, ok := r.Percent()
	if !ok {
		return notAvailable
	}
	return p + "%"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// headline builds the "nearest ceiling" line. The gap count is part of the
// line on purpose: a report with unmeasured limits must never read as an
// all-clear, and the headline is the one line everybody reads.
func headline(s ranking.Summary) string {
	gap := ""
	if s.NotMeasured > 0 {
		gap = fmt.Sprintf(" (%d %s not measured)", s.NotMeasured, plural(s.NotMeasured, "limit", "limits"))
	}
	switch {
	case s.HasNearest:
		n := s.Nearest
		pct, _ := n.Percent() // measured by construction
		detail := valueCell(n.Usage.Value, n.Unit) + " of " + valueCell(n.Limit.Value, n.Unit)
		if n.Unit != "" && n.Unit != unitBytes {
			detail += " " + n.Unit
		}
		return fmt.Sprintf("nearest ceiling: %s %s%% (%s)%s", cell(DisplayName(n)), cell(pct), cell(detail), gap)
	case s.Total == 0:
		return "nearest ceiling: none, no limits found"
	case s.NotMeasured > 0:
		return "nearest ceiling: unknown" + gap
	}
	return "nearest ceiling: none, every limit is unlimited"
}

// warnLine builds the second headline line, or "" when nothing is at or
// above the threshold.
func warnLine(s ranking.Summary, warn uint64) string {
	if s.Warned == 0 {
		return ""
	}
	return fmt.Sprintf("warning: %d %s at or above --warn %d%%",
		s.Warned, plural(s.Warned, "reading", "readings"), warn)
}

// TableOptions tunes [WriteTable].
type TableOptions struct {
	// Width is the terminal width in columns. Zero or less means unknown
	// (output is piped, or the width could not be read): nothing is cut.
	Width int
	// Wide shows Notes in full even when Width is known.
	Wide bool
	// Context is one extra line, such as "unit: a.service", printed after
	// the headline (and the warning line, when there is one) and before the
	// table. Empty prints no line.
	Context string
}

// minNotesColumns is how much of a Notes cell survives truncation, even when
// the line then overflows the terminal.
const minNotesColumns = 10

// ellipsis ends a truncated Notes cell. It is ASCII on purpose.
const ellipsis = "..."

// truncateRunes cuts s to at most limit runes, ending in "..." when it cut,
// and reports whether it did. limit is at least minNotesColumns, so the
// ellipsis always fits.
func truncateRunes(s string, limit int) (string, bool) {
	if utf8.RuneCountInString(s) <= limit {
		return s, false
	}
	r := []rune(s)
	return string(r[:limit-len(ellipsis)]) + ellipsis, true
}

// layout lays rows out as an aligned table and returns it with the trailing
// padding trimmed from every line.
func layout(rows [][]string) (string, error) {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	// Each row is written as tab-terminated cells. Every cell has already
	// been through cell(), so the only tabs and newlines tabwriter sees are
	// the ones added here.
	for _, cells := range rows {
		for _, c := range cells {
			// tabwriter returns write errors only from the underlying
			// writer, which is a bytes.Buffer here and cannot fail.
			_, _ = fmt.Fprint(tw, c, "\t")
		}
		_, _ = fmt.Fprintln(tw)
	}
	if err := tw.Flush(); err != nil {
		return "", fmt.Errorf("render table: %w", err)
	}
	// Padding is added after every tab-terminated cell, including the last,
	// so trim it rather than emit trailing whitespace on each line.
	var out strings.Builder
	for _, line := range strings.SplitAfter(buf.String(), "\n") {
		out.WriteString(strings.TrimRight(line, " \n"))
		if strings.HasSuffix(line, "\n") {
			out.WriteString("\n")
		}
	}
	return out.String(), nil
}

// WriteTable renders readings, which the caller has already ranked, as an
// aligned text table under a title line and the nearest-ceiling headline.
// warn is the --warn percentage; it only decides whether the warning line
// appears.
//
// Byte amounts (Unit "bytes") are humanized in the Limit, Usage and Headroom
// cells and in the headline; every other unit stays an exact integer.
//
// When opts.Width is known and opts.Wide is not set, only the Notes cells are
// cut to fit, ending in "...", keeping at least 10 columns of notes even if
// the line then overflows. Width is counted in runes after cell(). The
// headline, warning and context lines are never cut. If anything was cut, a
// last line says so, so hidden text is never silent.
func WriteTable(w io.Writer, readings []model.LimitReading, warn uint64, opts TableOptions) error {
	rows := make([][]string, 0, len(readings)+1)
	rows = append(rows, []string{"Name", "Source", "Limit", "Usage", "Headroom", "Used", "Unit", "Status", "Notes"})
	for _, r := range readings {
		rows = append(rows, []string{
			cell(DisplayName(r)),
			cell(r.Source),
			cell(limitCell(r)),
			cell(quantityCell(r.Usage, r.Unit)),
			cell(headroomCell(r)),
			cell(usedCell(r)),
			cell(r.Unit),
			cell(string(r.Status)),
			cell(r.Notes),
		})
	}
	table, err := layout(rows)
	if err != nil {
		return err
	}

	truncated := false
	if opts.Width > 0 && !opts.Wide {
		// Notes is the last column, so everything before it is the same
		// width on every line: the header line minus its own "Notes".
		// Cutting a Notes cell cannot move an earlier column.
		header, _, _ := strings.Cut(table, "\n")
		avail := max(opts.Width-(utf8.RuneCountInString(header)-len("Notes")), minNotesColumns)
		for _, cells := range rows[1:] {
			last := len(cells) - 1
			var cut bool
			if cells[last], cut = truncateRunes(cells[last], avail); cut {
				truncated = true
			}
		}
		if truncated {
			if table, err = layout(rows); err != nil {
				return err
			}
		}
	}

	var out strings.Builder
	out.WriteString("limits-explain\n")
	// The headline is built outside the tabwriter: it is prose, not a cell,
	// and must not influence the column widths. Its dynamic text goes
	// through cell() inside headline.
	sum := ranking.Summarize(readings, warn)
	out.WriteString(headline(sum) + "\n")
	if line := warnLine(sum, warn); line != "" {
		out.WriteString(line + "\n")
	}
	if opts.Context != "" {
		out.WriteString(cell(opts.Context) + "\n")
	}
	out.WriteString(table)
	if truncated {
		fmt.Fprintf(&out, "notes truncated to fit %d columns; --wide shows them in full\n", opts.Width)
	}
	if _, err := io.WriteString(w, out.String()); err != nil {
		return fmt.Errorf("write table: %w", err)
	}
	return nil
}

// jsonRow is the JSON shape of one reading: every LimitReading field plus
// Name, the same display string the table shows. Fields are declared in
// alphabetical order of their JSON keys, so the output is stable and
// diff-friendly.
//
// Headroom and OverBy are exact integers, null when the reading is not
// measured. Headroom is 0 when usage is at or past the limit, and OverBy is
// the excess (0 when not over). There are deliberately no float fields: the
// percentage is a display concern, and a consumer can derive it from the
// exact integers.
type jsonRow struct {
	Headroom model.Amount `json:"headroom"`
	Key      string       `json:"key"`
	Kind     model.Kind   `json:"kind"`
	Label    string       `json:"label"`
	Limit    model.Amount `json:"limit"`
	Name     string       `json:"name"`
	Notes    string       `json:"notes"`
	OverBy   model.Amount `json:"over_by"`
	Source   string       `json:"source"`
	Status   model.Status `json:"status"`
	Unit     string       `json:"unit"`
	Usage    model.Amount `json:"usage"`
}

// jsonSummary is the headline numbers. Nearest is the Key of the measured
// reading closest to its ceiling, or null when there is none.
type jsonSummary struct {
	Nearest     *string `json:"nearest"`
	NotMeasured int     `json:"not_measured"`
	Total       int     `json:"total"`
	Warned      int     `json:"warned"`
}

// jsonUnit is what the systemd step found. Name is null without a unit and
// Reason is null with one. DirectivesRead is false when there is no unit or
// its directives could not be read.
type jsonUnit struct {
	DirectivesRead bool    `json:"directives_read"`
	Name           *string `json:"name"`
	Reason         *string `json:"reason"`
}

// jsonEnvelope is the top-level JSON document, keys in alphabetical order.
type jsonEnvelope struct {
	PID      int         `json:"pid"`
	Readings []jsonRow   `json:"readings"`
	Summary  jsonSummary `json:"summary"`
	Unit     jsonUnit    `json:"unit"`
	Warn     uint64      `json:"warn"`
}

// JSONOptions is the context [WriteJSON] wraps around the readings.
type JSONOptions struct {
	// PID is the process that was reported.
	PID int
	// Warn is the --warn percentage in force.
	Warn uint64
	// Unit is what the systemd step did.
	Unit systemd.Outcome
}

// nonEmpty returns a pointer to s, or nil for "" so it renders as null.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// WriteJSON renders readings, in the order given (the CLI ranks them first),
// as an indented JSON envelope followed by a newline, for machine
// consumption: {pid, readings, summary, unit, warn}.
//
// Each reading carries every LimitReading field (including key, kind and
// label, not just the display name) plus name, so a consumer gets both the
// structured identity and the human string without rebuilding it, and the
// derived headroom and over_by. Unknown amounts are null; known ones are
// exact integers, never humanized. No readings render as [], never null.
func WriteJSON(w io.Writer, readings []model.LimitReading, opts JSONOptions) error {
	rows := make([]jsonRow, 0, len(readings))
	for _, r := range readings {
		headroom, overBy := model.UnknownAmount(), model.UnknownAmount()
		if rem, over, ok := r.Headroom(); ok {
			headroom, overBy = model.KnownAmount(rem), model.KnownAmount(over)
		}
		rows = append(rows, jsonRow{
			Headroom: headroom,
			Key:      r.Key,
			Kind:     r.Kind,
			Label:    r.Label,
			Limit:    r.Limit,
			Name:     DisplayName(r),
			Notes:    r.Notes,
			OverBy:   overBy,
			Source:   r.Source,
			Status:   r.Status,
			Unit:     r.Unit,
			Usage:    r.Usage,
		})
	}
	sum := ranking.Summarize(readings, opts.Warn)
	env := jsonEnvelope{
		PID:      opts.PID,
		Readings: rows,
		Summary: jsonSummary{
			NotMeasured: sum.NotMeasured,
			Total:       sum.Total,
			Warned:      sum.Warned,
		},
		Unit: jsonUnit{
			DirectivesRead: opts.Unit.Unit != "" && !opts.Unit.Failed,
			Name:           nonEmpty(opts.Unit.Unit),
			Reason:         nonEmpty(opts.Unit.Reason),
		},
		Warn: opts.Warn,
	}
	if sum.HasNearest {
		key := sum.Nearest.Key
		env.Summary.Nearest = &key
	}
	// An Encoder (rather than MarshalIndent) so HTML escaping can be turned
	// off: "<" and "&" in a kernel-derived label should stay readable, and
	// JSON does not need them escaped. Encode also appends the trailing
	// newline.
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	return nil
}
