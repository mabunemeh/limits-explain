package reporting

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mabunemeh/limits-explain/internal/model"
	"github.com/mabunemeh/limits-explain/internal/sources/systemd"
)

func reading(overrides func(*model.LimitReading)) model.LimitReading {
	r := model.LimitReading{
		Key:    "rlimit.nofile.single",
		Kind:   model.KindSingle,
		Label:  "open files",
		Source: "rlimit",
		Limit:  model.KnownAmount(1024),
		Usage:  model.UnknownAmount(),
		Unit:   "files",
		Status: model.StatusUnmeasured,
	}
	if overrides != nil {
		overrides(&r)
	}
	return r
}

func table(t *testing.T, readings ...model.LimitReading) string {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteTable(&buf, readings, 90, TableOptions{}); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	return buf.String()
}

func TestLimitCell(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*model.LimitReading)
		want string
	}{
		{"unlimited when status says so", func(r *model.LimitReading) {
			r.Limit, r.Status = model.UnknownAmount(), model.StatusUnlimited
		}, "unlimited"},
		// An unknown limit is "unlimited" ONLY when the kernel was observed
		// saying so. For an unparseable row it is genuinely unknown, and
		// "unlimited" would assert a ceiling that was never read.
		{"unparseable is n/a, not unlimited", func(r *model.LimitReading) {
			r.Limit, r.Status, r.Label = model.UnknownAmount(), model.StatusUnparseable, ""
		}, "n/a"},
		{"unreadable is n/a", func(r *model.LimitReading) {
			r.Limit, r.Status = model.UnknownAmount(), model.StatusUnreadable
		}, "n/a"},
		{"known limit is the number", nil, "1024"},
		{"known zero is zero", func(r *model.LimitReading) { r.Limit = model.KnownAmount(0) }, "0"},
		{"above int64 is exact", func(r *model.LimitReading) {
			r.Limit = model.KnownAmount(18446744073709551614)
		}, "18446744073709551614"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := limitCell(reading(tc.mod)); got != tc.want {
				t.Errorf("limitCell = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDisplayName(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*model.LimitReading)
		want string
	}{
		{"single is the bare label", nil, "open files"},
		{"soft is suffixed", func(r *model.LimitReading) { r.Kind = model.KindSoft }, "open files (soft)"},
		{"hard is suffixed", func(r *model.LimitReading) { r.Kind = model.KindHard }, "open files (hard)"},
		{"empty label gets a placeholder", func(r *model.LimitReading) { r.Label = "" }, "(unparseable row)"},
		{"empty label ignores kind", func(r *model.LimitReading) {
			r.Label, r.Kind = "", model.KindSoft
		}, "(unparseable row) (soft)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DisplayName(reading(tc.mod)); got != tc.want {
				t.Errorf("DisplayName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTableLayout(t *testing.T) {
	out := table(t,
		reading(func(r *model.LimitReading) { r.Kind = model.KindSoft }),
		reading(func(r *model.LimitReading) {
			r.Kind, r.Limit, r.Status = model.KindHard, model.UnknownAmount(), model.StatusUnlimited
		}),
	)
	want := "limits-explain\n" +
		"nearest ceiling: unknown (1 limit not measured)\n" +
		"Name               Source  Limit      Usage  Headroom  Used  Unit   Status      Notes\n" +
		"open files (soft)  rlimit  1024       n/a    n/a       n/a   files  unmeasured\n" +
		"open files (hard)  rlimit  unlimited  n/a    n/a       n/a   files  unlimited\n"
	if out != want {
		t.Errorf("table mismatch\n got:\n%s\nwant:\n%s", out, want)
	}
}

func TestTableHasNoTrailingWhitespace(t *testing.T) {
	out := table(t, reading(nil))
	for _, line := range strings.Split(out, "\n") {
		if line != strings.TrimRight(line, " ") {
			t.Errorf("line %q has trailing whitespace", line)
		}
	}
}

func TestTableEmpty(t *testing.T) {
	out := table(t)
	want := "limits-explain\n" +
		"nearest ceiling: none, no limits found\n" +
		"Name  Source  Limit  Usage  Headroom  Used  Unit  Status  Notes\n"
	if out != want {
		t.Errorf("empty table = %q, want %q", out, want)
	}
}

func TestTableUnparseableRowDoesNotClaimUnlimited(t *testing.T) {
	out := table(t, reading(func(r *model.LimitReading) {
		r.Limit, r.Status, r.Label = model.UnknownAmount(), model.StatusUnparseable, ""
		r.Notes = `unparseable: "x"`
	}))
	var row string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "(unparseable row)") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("unparseable row missing from table:\n%s", out)
	}
	if !strings.Contains(row, "n/a") {
		t.Errorf("row %q has no n/a", row)
	}
	if strings.Contains(row, "unlimited") {
		t.Errorf("row %q claims unlimited", row)
	}
}

func TestTableMarkupIsPrintedLiterally(t *testing.T) {
	// The kernel-derived name is rendered as-is: there is no markup layer to
	// interpret "[/red]" or "[bold", so nothing may be added or removed.
	out := table(t, reading(func(r *model.LimitReading) { r.Label = "weird [/red]evil[bold thing" }))
	if !strings.Contains(out, "weird [/red]evil[bold thing") {
		t.Errorf("brackets not printed literally:\n%s", out)
	}
}

func TestTableNeutralisesLayoutBreakers(t *testing.T) {
	tests := []struct {
		name  string
		label string
	}{
		{"tab", "open\tfiles"},
		{"newline", "open\nfiles"},
		{"carriage return", "open\rfiles"},
		{"form feed", "open\ffiles"},
		{"vertical tab", "open\vfiles"},
		{"escape byte", "open\x1b[31mfiles"},
		{"nul", "open\x00files"},
		{"c1 control", "open\u009bfiles"},
		{"line separator", "open\u2028files"},
		{"bidi override", "open\u202efiles"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := table(t,
				reading(func(r *model.LimitReading) { r.Label = tc.label; r.Notes = tc.label }),
				reading(nil),
			)

			// Exactly the title, the headline, the header and two rows: a
			// newline in a cell must not add a line.
			if got := strings.Count(out, "\n"); got != 5 {
				t.Errorf("table has %d lines, want 5:\n%q", got, out)
			}
			if strings.ContainsAny(out, "\t\r\x1b\x00\f\v\u009b\u2028\u202e") {
				t.Errorf("control character survived in %q", out)
			}

			// And the layout survived: both rows put Source in the same
			// column as the header does.
			lines := strings.Split(out, "\n")
			col := runeIndex(lines[2], "Source")
			for _, line := range lines[3:5] {
				if got := runeIndex(line, "rlimit"); got != col {
					t.Errorf("Source column at %d, header at %d:\n%s", got, col, out)
				}
			}
			if !strings.Contains(out, "open\ufffd") && !strings.Contains(out, "\ufffdfiles") {
				t.Errorf("no replacement character in %q", out)
			}
		})
	}
}

// runeIndex is strings.Index measured in runes, which is how tabwriter
// measures cell width.
func runeIndex(s, substr string) int {
	i := strings.Index(s, substr)
	if i < 0 {
		return -1
	}
	return len([]rune(s[:i]))
}

func TestTableReplacesInvalidUTF8(t *testing.T) {
	out := table(t, reading(func(r *model.LimitReading) { r.Label = "bad\xffbyte" }))
	if !strings.Contains(out, "bad\ufffdbyte") {
		t.Errorf("invalid UTF-8 not replaced: %q", out)
	}
}

func TestTableKeepsOrdinaryUnicode(t *testing.T) {
	out := table(t, reading(func(r *model.LimitReading) { r.Label = "caf\u00e9 \u65e5\u672c" }))
	if !strings.Contains(out, "caf\u00e9 \u65e5\u672c") {
		t.Errorf("ordinary unicode was altered: %q", out)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWritersReportWriteErrors(t *testing.T) {
	if err := WriteTable(failWriter{}, nil, 90, TableOptions{}); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("WriteTable error = %v", err)
	}
	if err := WriteJSON(failWriter{}, nil, JSONOptions{}); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("WriteJSON error = %v", err)
	}
}

func TestJSONShape(t *testing.T) {
	var buf bytes.Buffer
	err := WriteJSON(&buf, []model.LimitReading{
		reading(func(r *model.LimitReading) {
			r.Key, r.Kind = "rlimit.fsize.soft", model.KindSoft
			r.Limit = model.KnownAmount(18446744073709551614)
		}),
		reading(func(r *model.LimitReading) {
			r.Key, r.Kind, r.Limit, r.Status = "rlimit.fsize.hard", model.KindHard, model.UnknownAmount(), model.StatusUnlimited
		}),
	}, JSONOptions{PID: 4242, Warn: 90, Unit: systemd.Outcome{Reason: "systemd is not consulted"}})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// Nothing is measured and there is no unit, so summary.nearest,
	// unit.name are null; the reason is set.
	want := `{
  "pid": 4242,
  "readings": [
    {
      "headroom": null,
      "key": "rlimit.fsize.soft",
      "kind": "soft",
      "label": "open files",
      "limit": 18446744073709551614,
      "name": "open files (soft)",
      "notes": "",
      "over_by": null,
      "source": "rlimit",
      "status": "unmeasured",
      "unit": "files",
      "usage": null
    },
    {
      "headroom": null,
      "key": "rlimit.fsize.hard",
      "kind": "hard",
      "label": "open files",
      "limit": null,
      "name": "open files (hard)",
      "notes": "",
      "over_by": null,
      "source": "rlimit",
      "status": "unlimited",
      "unit": "files",
      "usage": null
    }
  ],
  "summary": {
    "nearest": null,
    "not_measured": 1,
    "total": 2,
    "warned": 0
  },
  "unit": {
    "directives_read": false,
    "name": null,
    "reason": "systemd is not consulted"
  },
  "warn": 90
}
`
	if out != want {
		t.Errorf("json mismatch\n got:\n%s\nwant:\n%s", out, want)
	}
	if !json.Valid([]byte(out)) {
		t.Error("output is not valid JSON")
	}
}

func TestJSONEnvelopeSummaryAndUnit(t *testing.T) {
	// The nearest ceiling is the first row, so its key is the one reported.
	rows := []model.LimitReading{measured("tasks", 100, 95), measured("memory", 100, 10), reading(nil)}
	rows[0].Key = "cgroup.pids.max"
	tests := []struct {
		name string
		unit systemd.Outcome
		want string // the summary and unit blocks, then warn
	}{
		{"unit read", systemd.Outcome{Unit: "a.service"}, `  "summary": {
    "nearest": "cgroup.pids.max",
    "not_measured": 1,
    "total": 3,
    "warned": 1
  },
  "unit": {
    "directives_read": true,
    "name": "a.service",
    "reason": null
  },
  "warn": 90
}
`},
		{"unit found, directives failed", systemd.Outcome{Unit: "a.service", Failed: true}, `  "summary": {
    "nearest": "cgroup.pids.max",
    "not_measured": 1,
    "total": 3,
    "warned": 1
  },
  "unit": {
    "directives_read": false,
    "name": "a.service",
    "reason": null
  },
  "warn": 90
}
`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteJSON(&buf, rows, JSONOptions{PID: 1, Warn: 90, Unit: tc.unit}); err != nil {
				t.Fatal(err)
			}
			_, got, ok := strings.Cut(buf.String(), "  ],\n")
			if !ok || got != tc.want {
				t.Errorf("tail mismatch\n got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestJSONEmptyReadingsIsArrayNotNull(t *testing.T) {
	for _, in := range [][]model.LimitReading{nil, {}} {
		var buf bytes.Buffer
		if err := WriteJSON(&buf, in, JSONOptions{PID: 7, Warn: 80}); err != nil {
			t.Fatal(err)
		}
		if want := `"readings": [],`; !strings.Contains(buf.String(), want) {
			t.Errorf("WriteJSON(%v) = %q, want it to contain %q", in, buf.String(), want)
		}
	}
}

func TestJSONKeepsControlCharsAsEscapesAndHTMLReadable(t *testing.T) {
	var buf bytes.Buffer
	label := "a<b>&\x1b\t\"q\""
	err := WriteJSON(&buf, []model.LimitReading{reading(func(r *model.LimitReading) { r.Label = label })}, JSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Readings []map[string]any `json:"readings"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	rows := env.Readings
	// JSON round-trips the raw string exactly; sanitising is a terminal
	// concern and does not apply here.
	if rows[0]["label"] != label {
		t.Errorf("label = %q, want %q", rows[0]["label"], label)
	}
	if !strings.Contains(buf.String(), "a<b>&") {
		t.Errorf("HTML characters were escaped: %s", buf.String())
	}
}
