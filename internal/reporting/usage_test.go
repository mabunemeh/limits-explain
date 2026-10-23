package reporting

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mabunemeh/limits-explain/internal/model"
)

// measured builds a measured reading with the given label, limit and usage.
func measured(label string, limit, usage uint64) model.LimitReading {
	return reading(func(r *model.LimitReading) {
		r.Label = label
		r.Limit, r.Usage = model.KnownAmount(limit), model.KnownAmount(usage)
		r.Status = model.StatusOK
	})
}

func unlimited(label string) model.LimitReading {
	return reading(func(r *model.LimitReading) {
		r.Label = label
		r.Limit, r.Status = model.UnknownAmount(), model.StatusUnlimited
	})
}

func tableWarn(t *testing.T, warn uint64, readings ...model.LimitReading) string {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteTable(&buf, readings, warn); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	return buf.String()
}

func TestTableExactLayout(t *testing.T) {
	out := tableWarn(t, 90,
		measured("memory", 1000, 1125),
		measured("tasks", 100, 62),
		unlimited("open files"),
	)
	want := "limits-explain\n" +
		"nearest ceiling: memory 112.5% (1125 of 1000 files)\n" +
		"warning: 1 reading at or above --warn 90%\n" +
		"Name        Source  Limit      Usage  Headroom     Used    Unit   Status     Notes\n" +
		"memory      rlimit  1000       1125   over by 125  112.5%  files  ok\n" +
		"tasks       rlimit  100        62     38           62.0%   files  ok\n" +
		"open files  rlimit  unlimited  n/a    n/a          n/a     files  unlimited\n"
	if out != want {
		t.Errorf("table mismatch\n got:\n%s\nwant:\n%s", out, want)
	}
}

func TestHeadline(t *testing.T) {
	gap := func(label string) model.LimitReading {
		return reading(func(r *model.LimitReading) { r.Label = label })
	}
	tests := []struct {
		name string
		in   []model.LimitReading
		warn uint64
		want string // the lines between the title and the column header
	}{
		{"measured under the threshold", []model.LimitReading{measured("tasks", 100, 62)}, 90,
			"nearest ceiling: tasks 62.0% (62 of 100 files)\n"},
		{"gap count, plural", []model.LimitReading{measured("tasks", 100, 62), gap("a"), gap("b")}, 90,
			"nearest ceiling: tasks 62.0% (62 of 100 files) (2 limits not measured)\n"},
		{"gap count, singular", []model.LimitReading{measured("tasks", 100, 62), gap("a")}, 90,
			"nearest ceiling: tasks 62.0% (62 of 100 files) (1 limit not measured)\n"},
		{"unlimited rows are not gaps", []model.LimitReading{measured("tasks", 100, 62), unlimited("u")}, 90,
			"nearest ceiling: tasks 62.0% (62 of 100 files)\n"},
		{"nothing measured", []model.LimitReading{gap("a"), gap("b"), unlimited("u")}, 90,
			"nearest ceiling: unknown (2 limits not measured)\n"},
		{"nothing measured, one gap", []model.LimitReading{gap("a")}, 90,
			"nearest ceiling: unknown (1 limit not measured)\n"},
		{"all unlimited", []model.LimitReading{unlimited("a"), unlimited("b")}, 90,
			"nearest ceiling: none, every limit is unlimited\n"},
		{"empty", nil, 90, "nearest ceiling: none, no limits found\n"},
		{"warn line, one", []model.LimitReading{measured("tasks", 10, 9)}, 90,
			"nearest ceiling: tasks 90.0% (9 of 10 files)\nwarning: 1 reading at or above --warn 90%\n"},
		{"warn line, several", []model.LimitReading{measured("a", 10, 10), measured("b", 10, 9), measured("c", 10, 1)}, 90,
			"nearest ceiling: a 100.0% (10 of 10 files)\nwarning: 2 readings at or above --warn 90%\n"},
		{"just under the threshold: floored display, no warning", []model.LimitReading{measured("a", 10000000, 8999999)}, 90,
			"nearest ceiling: a 89.9% (8999999 of 10000000 files)\n"},
		{"warn value is echoed", []model.LimitReading{measured("a", 100, 50)}, 50,
			"nearest ceiling: a 50.0% (50 of 100 files)\nwarning: 1 reading at or above --warn 50%\n"},
		{"zero limit", []model.LimitReading{measured("a", 0, 0)}, 90,
			"nearest ceiling: a 100.0% (0 of 0 files)\nwarning: 1 reading at or above --warn 90%\n"},
		{"above 2^53", []model.LimitReading{measured("a", 1<<60+1, 1<<60)}, 100,
			"nearest ceiling: a 99.9% (1152921504606846976 of 1152921504606846977 files)\n"},
		{"no unit", []model.LimitReading{reading(func(r *model.LimitReading) {
			r.Label, r.Unit = "a", ""
			r.Limit, r.Usage, r.Status = model.KnownAmount(4), model.KnownAmount(1), model.StatusOK
		})}, 90, "nearest ceiling: a 25.0% (1 of 4)\n"},
		{"names are sanitised", []model.LimitReading{measured("bad\x1b[31m\nname", 100, 5)}, 90,
			"nearest ceiling: bad\ufffd[31m\ufffdname 5.0% (5 of 100 files)\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := tableWarn(t, tc.warn, tc.in...)
			lines := strings.SplitAfter(out, "\n")
			if lines[0] != "limits-explain\n" {
				t.Fatalf("first line = %q", lines[0])
			}
			var got strings.Builder
			for _, l := range lines[1:] {
				if strings.HasPrefix(l, "Name") {
					break
				}
				got.WriteString(l)
			}
			if got.String() != tc.want {
				t.Errorf("headline block = %q, want %q", got.String(), tc.want)
			}
		})
	}
}

func TestHeadroomCell(t *testing.T) {
	tests := []struct {
		name string
		r    model.LimitReading
		want string
	}{
		{"unknown", reading(nil), "n/a"},
		{"under", measured("a", 100, 62), "38"},
		{"exactly at the limit is 0, not over", measured("a", 100, 100), "0"},
		{"over", measured("a", 100, 101), "over by 1"},
		{"zero limit, zero usage", measured("a", 0, 0), "0"},
		{"zero limit, some usage", measured("a", 0, 5), "over by 5"},
		{"over by a lot, no wraparound", measured("a", 1, 18446744073709551615), "over by 18446744073709551614"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := headroomCell(tc.r); got != tc.want {
				t.Errorf("headroomCell = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUsedCell(t *testing.T) {
	if got := usedCell(reading(nil)); got != "n/a" {
		t.Errorf("unmeasured = %q", got)
	}
	if got := usedCell(measured("a", 1000, 620)); got != "62.0%" {
		t.Errorf("measured = %q", got)
	}
}

func TestJSONHeadroomAndOverBy(t *testing.T) {
	var buf bytes.Buffer
	err := WriteJSON(&buf, []model.LimitReading{
		measured("over", 1000, 1125),
		measured("under", 100, 62),
		measured("exact", 7, 7),
		reading(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := decode(t, buf.Bytes())
	tests := []struct{ headroom, overBy string }{
		{"0", "125"}, {"38", "0"}, {"0", "0"}, {"null", "null"},
	}
	for i, want := range tests {
		if rows[i]["headroom"] != want.headroom || rows[i]["over_by"] != want.overBy {
			t.Errorf("row %d: headroom %s over_by %s, want %s %s",
				i, rows[i]["headroom"], rows[i]["over_by"], want.headroom, want.overBy)
		}
	}
}

func TestJSONKeepsGivenOrderAndAlphabeticalFields(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, []model.LimitReading{measured("zzz", 10, 1), measured("aaa", 10, 2)}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Index(out, `"zzz"`) > strings.Index(out, `"aaa"`) {
		t.Errorf("WriteJSON reordered its input:\n%s", out)
	}
	last := -1
	for _, f := range []string{"headroom", "key", "kind", "label", "limit", "name", "notes", "over_by", "source", "status", "unit", "usage"} {
		i := strings.Index(out, `"`+f+`":`)
		if i < 0 || i < last {
			t.Fatalf("field %q missing or out of alphabetical order in:\n%s", f, out)
		}
		last = i
	}
}

// decode unmarshals a JSON array of objects keeping each value as its raw
// text, so numbers are compared exactly as written.
func decode(t *testing.T, data []byte) []map[string]string {
	t.Helper()
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, data)
	}
	rows := make([]map[string]string, len(raw))
	for i, r := range raw {
		rows[i] = map[string]string{}
		for k, v := range r {
			rows[i][k] = string(v)
		}
	}
	return rows
}
