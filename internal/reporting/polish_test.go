package reporting

import (
	"bytes"
	"math"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mabunemeh/limits-explain/internal/model"
)

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		name string
		in   uint64
		want string
	}{
		{"zero", 0, "0 B"},
		{"one", 1, "1 B"},
		{"top of plain bytes", 1023, "1023 B"},
		{"one KiB", 1024, "1.0 KiB"},
		{"floored, not rounded (1.4995)", 1535, "1.4 KiB"},
		{"1.5 KiB", 1536, "1.5 KiB"},
		{"just under 2 KiB floors to 1.9", 2047, "1.9 KiB"},
		{"just under 1 MiB stays in KiB", 1048575, "1023.9 KiB"},
		{"one MiB", 1048576, "1.0 MiB"},
		{"64 MiB", 67108864, "64.0 MiB"},
		{"just under 1 GiB", 1<<30 - 1, "1023.9 MiB"},
		{"one GiB", 1 << 30, "1.0 GiB"},
		{"one TiB", 1 << 40, "1.0 TiB"},
		{"one PiB", 1 << 50, "1.0 PiB"},
		{"one EiB", 1 << 60, "1.0 EiB"},
		{"just under 1 EiB", 1<<60 - 1, "1023.9 PiB"},
		{"RLIM_INFINITY-1", 18446744073709551614, "15.9 EiB"},
		{"max uint64", math.MaxUint64, "15.9 EiB"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := humanBytes(tc.in); got != tc.want {
				t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A value below its limit must never print equal to the limit.
func TestHumanBytesNeverRoundsUpToTheLimit(t *testing.T) {
	for _, limit := range []uint64{1024, 1 << 20, 1 << 30, 1 << 40, 1 << 50, 1 << 60} {
		if a, b := humanBytes(limit-1), humanBytes(limit); a == b {
			t.Errorf("humanBytes(%d) == humanBytes(%d) = %q", limit-1, limit, a)
		}
	}
}

func bytesReading(label string, limit, usage uint64) model.LimitReading {
	r := measured(label, limit, usage)
	r.Unit = "bytes"
	return r
}

func TestBytesHumanizedInTheTableOnly(t *testing.T) {
	out := table(t,
		bytesReading("memory", 67108864, 1572864),
		bytesReading("over", 1000, 1125),
		measured("tasks", 100, 62), // files: stays exact
	)
	if !strings.Contains(out, "nearest ceiling: over 112.5% (1.0 KiB of 1000 B)\n") {
		t.Errorf("headline not humanized:\n%s", out)
	}
	// Cells joined by single spaces: Limit, Usage and Headroom are humanized
	// for bytes, exact for other units, and Used and Unit are unchanged.
	var got []string
	for _, l := range tableLines(out)[1:] {
		got = append(got, strings.Join(strings.Fields(l), " "))
	}
	want := []string{
		"memory rlimit 64.0 MiB 1.5 MiB 62.5 MiB 2.3% bytes ok",
		"over rlimit 1000 B 1.0 KiB over by 125 B 112.5% bytes ok",
		"tasks rlimit 100 62 38 62.0% files ok",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// JSON keeps exact integers.
	var buf bytes.Buffer
	if err := WriteJSON(&buf, []model.LimitReading{bytesReading("memory", 67108864, 1572864)}, JSONOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"limit": 67108864`, `"usage": 1572864`, `"headroom": 65536000`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("json lacks %q:\n%s", want, buf.String())
		}
	}
	if strings.Contains(buf.String(), "MiB") {
		t.Errorf("json was humanized:\n%s", buf.String())
	}
}

func TestHeadroomHumanizedOnlyForBytes(t *testing.T) {
	tests := []struct {
		name string
		r    model.LimitReading
		want string
	}{
		{"bytes remaining", bytesReading("a", 4096, 1024), "3.0 KiB"},
		{"bytes at the limit", bytesReading("a", 4096, 4096), "0 B"},
		{"bytes over by", bytesReading("a", 1024, 3072), "over by 2.0 KiB"},
		{"bytes over by under 1 KiB", bytesReading("a", 100, 150), "over by 50 B"},
		{"bytes unknown stays n/a", func() model.LimitReading {
			r := bytesReading("a", 100, 1)
			r.Usage = model.UnknownAmount()
			return r
		}(), "n/a"},
		{"other units exact", measured("a", 100000, 1), "99999"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := headroomCell(tc.r); got != tc.want {
				t.Errorf("headroomCell = %q, want %q", got, tc.want)
			}
		})
	}
}

// eAcute is a two-byte rune, built without a non-ASCII literal.
var eAcute = string(rune(0xe9))

const longNotes = "nearest its ceiling: /a (sys/fs/cgroup/a/memory.max); usage read at /a (sys/fs/cgroup/a/memory.current)"

func notesReading(label, notes string) model.LimitReading {
	r := measured(label, 100, 5)
	r.Notes = notes
	return r
}

func renderWith(t *testing.T, opts TableOptions, rows ...model.LimitReading) string {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteTable(&buf, rows, 90, opts); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	return buf.String()
}

// tableLines returns the header line and the rows, without the trailing
// truncation line.
func tableLines(out string) []string {
	var lines []string
	started := false
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(l, "Name") {
			started = true
		}
		if started && !strings.HasPrefix(l, "notes truncated") {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestTruncationExactLayout(t *testing.T) {
	// The columns before Notes take 60 columns here, so 80 leaves 20 for the
	// notes: 17 kept plus "...".
	got := renderWith(t, TableOptions{Width: 80}, notesReading("tasks", longNotes))
	want := "limits-explain\n" +
		"nearest ceiling: tasks 5.0% (5 of 100 files)\n" +
		"Name   Source  Limit  Usage  Headroom  Used  Unit   Status  Notes\n" +
		"tasks  rlimit  100    5      95        5.0%  files  ok      nearest its ceili...\n" +
		"notes truncated to fit 80 columns; --wide shows them in full\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTruncationAtSeveralWidths(t *testing.T) {
	rows := []model.LimitReading{
		notesReading("tasks", longNotes),
		notesReading("short", "fits"),
		notesReading("empty", ""),
	}
	full := tableLines(renderWith(t, TableOptions{}, rows...))
	prefix := utf8.RuneCountInString(full[0]) - len("Notes")

	tests := []struct {
		name      string
		width     int
		wantNotes int // runes kept in the long Notes cell
		cut       bool
	}{
		{"roomy", 400, len(longNotes), false},
		{"just fits", prefix + len(longNotes), len(longNotes), false},
		{"one short", prefix + len(longNotes) - 1, len(longNotes) - 1, true},
		{"narrow", prefix + 30, 30, true},
		{"exactly the minimum", prefix + 10, 10, true},
		{"below the minimum", prefix + 5, 10, true},
		{"tiny terminal", 20, 10, true},
		{"width one", 1, 10, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := renderWith(t, TableOptions{Width: tc.width}, rows...)
			lines := tableLines(out)
			notes := strings.TrimPrefix(lines[1], full[1][:prefix])
			if got := utf8.RuneCountInString(notes); got != tc.wantNotes {
				t.Errorf("notes = %q (%d runes), want %d runes", notes, got, tc.wantNotes)
			}
			if tc.cut != strings.HasSuffix(notes, "...") {
				t.Errorf("ellipsis presence = %v, want %v: %q", !tc.cut, tc.cut, notes)
			}
			if !tc.cut && notes != longNotes {
				t.Errorf("uncut notes changed: %q", notes)
			}
			// Short notes are never touched.
			if !strings.HasSuffix(lines[2], "fits") {
				t.Errorf("short notes changed: %q", lines[2])
			}
			wantLine := "notes truncated to fit " + strconv.Itoa(tc.width) + " columns; --wide shows them in full\n"
			if tc.cut != strings.HasSuffix(out, wantLine) {
				t.Errorf("truncation line present = %v, want %v:\n%s", !tc.cut, tc.cut, out)
			}
			if !tc.cut && strings.Contains(out, "truncated") {
				t.Errorf("truncation line printed with nothing cut:\n%s", out)
			}
			// Columns before Notes are never moved by the cut.
			for i := range lines {
				if a, b := head(lines[i], prefix), head(full[i], prefix); a != b {
					t.Errorf("line %d prefix changed: %q vs %q", i, a, b)
				}
			}
		})
	}
}

// head returns the first n bytes of s, or all of s when it is shorter.
func head(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func TestWideAndUnknownWidthNeverTruncate(t *testing.T) {
	rows := []model.LimitReading{notesReading("tasks", longNotes)}
	tests := []struct {
		name string
		opts TableOptions
	}{
		{"wide with a known width", TableOptions{Width: 40, Wide: true}},
		{"unknown width", TableOptions{}},
		{"zero width", TableOptions{Width: 0}},
		{"negative width", TableOptions{Width: -1}},
		{"wide and unknown", TableOptions{Wide: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := renderWith(t, tc.opts, rows...)
			if !strings.Contains(out, longNotes) || strings.Contains(out, "truncated") || strings.Contains(out, "...") {
				t.Errorf("notes were cut:\n%s", out)
			}
		})
	}
}

func TestTruncationLeavesOtherLinesAlone(t *testing.T) {
	long := strings.Repeat("x", 300)
	r := notesReading(long, long)
	out := renderWith(t, TableOptions{Width: 40, Context: "unit: " + long}, r, measured("memory", 10, 10))
	for _, want := range []string{long + "  rlimit", "warning: 1 reading", "unit: " + long + "\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("a non-notes line was cut, lacks %.20q:\n%s", want, out)
		}
	}
}

func TestTruncationCountsRunesNotBytes(t *testing.T) {
	// Multi-byte runes must be cut on a rune boundary and counted as one
	// column each.
	notes := strings.Repeat(eAcute, 40)
	out := renderWith(t, TableOptions{Width: 80}, notesReading("tasks", notes))
	if !utf8.ValidString(out) {
		t.Fatalf("output is not valid UTF-8: %q", out)
	}
	prefix := utf8.RuneCountInString(tableLines(renderWith(t, TableOptions{}, notesReading("tasks", notes)))[0]) - len("Notes")
	want := strings.Repeat(eAcute, 80-prefix-3) + "..."
	if !strings.Contains(out, want+"\n") {
		t.Errorf("rune cut wrong, want suffix %q in:\n%s", want, out)
	}
}
