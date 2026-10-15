// Package reporting renders limit readings as a text table or as JSON.
package reporting

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/mabunemeh/limits-explain/internal/model"
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

func amountCell(a model.Amount) string {
	if !a.Known {
		return notAvailable
	}
	return strconv.FormatUint(a.Value, 10)
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
		return amountCell(r.Limit)
	}
	if r.Status == model.StatusUnlimited {
		return string(model.StatusUnlimited)
	}
	return notAvailable
}

// WriteTable renders readings as an aligned text table under a title line.
func WriteTable(w io.Writer, readings []model.LimitReading) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)

	// Each row is written as tab-terminated cells. Every cell has already
	// been through cell(), so the only tabs and newlines tabwriter sees are
	// the ones added here.
	row := func(cells ...string) {
		for _, c := range cells {
			// tabwriter returns write errors only from the underlying
			// writer, which is a bytes.Buffer here and cannot fail.
			_, _ = fmt.Fprint(tw, c, "\t")
		}
		_, _ = fmt.Fprintln(tw)
	}

	row("Name", "Source", "Limit", "Usage", "Unit", "Status", "Notes")
	for _, r := range readings {
		row(
			cell(DisplayName(r)),
			cell(r.Source),
			cell(limitCell(r)),
			cell(amountCell(r.Usage)),
			cell(r.Unit),
			cell(string(r.Status)),
			cell(r.Notes),
		)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("render table: %w", err)
	}

	// Padding is added after every tab-terminated cell, including the last,
	// so trim it rather than emit trailing whitespace on each line.
	var out strings.Builder
	out.WriteString("limits-explain\n")
	for _, line := range strings.SplitAfter(buf.String(), "\n") {
		out.WriteString(strings.TrimRight(line, " \n"))
		if strings.HasSuffix(line, "\n") {
			out.WriteString("\n")
		}
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
type jsonRow struct {
	Key    string       `json:"key"`
	Kind   model.Kind   `json:"kind"`
	Label  string       `json:"label"`
	Limit  model.Amount `json:"limit"`
	Name   string       `json:"name"`
	Notes  string       `json:"notes"`
	Source string       `json:"source"`
	Status model.Status `json:"status"`
	Unit   string       `json:"unit"`
	Usage  model.Amount `json:"usage"`
}

// WriteJSON renders readings as an indented JSON array followed by a newline,
// for machine consumption.
//
// Each object carries every LimitReading field (including key, kind and
// label, not just the display name) plus name, so a consumer gets both the
// structured identity and the human string without rebuilding it. Unknown
// amounts are null; known ones are exact integers. Empty input renders as
// [], never null.
func WriteJSON(w io.Writer, readings []model.LimitReading) error {
	rows := make([]jsonRow, 0, len(readings))
	for _, r := range readings {
		rows = append(rows, jsonRow{
			Key:    r.Key,
			Kind:   r.Kind,
			Label:  r.Label,
			Limit:  r.Limit,
			Name:   DisplayName(r),
			Notes:  r.Notes,
			Source: r.Source,
			Status: r.Status,
			Unit:   r.Unit,
			Usage:  r.Usage,
		})
	}
	// An Encoder (rather than MarshalIndent) so HTML escaping can be turned
	// off: "<" and "&" in a kernel-derived label should stay readable, and
	// JSON does not need them escaped. Encode also appends the trailing
	// newline.
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rows); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	return nil
}
