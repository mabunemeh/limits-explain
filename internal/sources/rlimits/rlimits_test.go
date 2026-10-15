package rlimits

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mabunemeh/limits-explain/internal/model"
	"github.com/mabunemeh/limits-explain/internal/sources"
)

const pid = 4242

const header = "Limit                     Soft Limit           Hard Limit           Units\n"

// fixture returns an fs.FS rooted at the named committed fixture tree. The
// trees are verbatim kernel captures and hand-built edge cases; tests never
// mock filesystem reads, they point the fs.FS seam at these instead.
func fixture(t *testing.T, name string) fs.FS {
	t.Helper()
	fsys := os.DirFS("testdata/" + name)
	if _, err := fs.Stat(fsys, "proc/4242/limits"); err != nil {
		t.Fatalf("fixture %q: %v", name, err)
	}
	return fsys
}

// inMemory builds an fs.FS holding a single proc/4242/limits file.
func inMemory(content string) fs.FS {
	return fstest.MapFS{"proc/4242/limits": {Data: []byte(content)}}
}

func collect(t *testing.T, fsys fs.FS) map[string]model.LimitReading {
	t.Helper()
	readings, err := New().Collect(pid, fsys)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return byKey(t, readings)
}

func byKey(t *testing.T, readings []model.LimitReading) map[string]model.LimitReading {
	t.Helper()
	m := make(map[string]model.LimitReading, len(readings))
	for _, r := range readings {
		if _, dup := m[r.Key]; dup {
			t.Fatalf("duplicate key %q in output", r.Key)
		}
		m[r.Key] = r
	}
	return m
}

func mustGet(t *testing.T, m map[string]model.LimitReading, key string) model.LimitReading {
	t.Helper()
	r, ok := m[key]
	if !ok {
		t.Fatalf("missing key %q", key)
	}
	return r
}

func wantLimit(t *testing.T, r model.LimitReading, want model.Amount) {
	t.Helper()
	if r.Limit != want {
		t.Errorf("%s: Limit = %+v, want %+v", r.Key, r.Limit, want)
	}
}

func TestParsesRealCapture(t *testing.T) {
	// testdata/basic is a real `cat /proc/self/limits` capture from a WSL2
	// Ubuntu 22.04 host, so these assertions match that capture exactly.
	m := collect(t, fixture(t, "basic"))

	// soft != hard gives two readings, split by kind.
	soft := mustGet(t, m, "rlimit.nofile.soft")
	wantLimit(t, soft, model.KnownAmount(10240))
	wantLimit(t, mustGet(t, m, "rlimit.nofile.hard"), model.KnownAmount(1048576))
	if soft.Label != "open files" || soft.Unit != "files" || soft.Source != "rlimit" {
		t.Errorf("nofile.soft = %+v", soft)
	}
	if soft.Kind != model.KindSoft || soft.Usage.Known || soft.Status != model.StatusUnmeasured {
		t.Errorf("nofile.soft kind/usage/status = %q/%+v/%q", soft.Kind, soft.Usage, soft.Status)
	}

	// soft == hard collapses to a single reading.
	nproc := mustGet(t, m, "rlimit.nproc.single")
	wantLimit(t, nproc, model.KnownAmount(62339))
	if nproc.Unit != "processes" || nproc.Kind != model.KindSingle {
		t.Errorf("nproc.single = %+v", nproc)
	}

	// unlimited is an unknown amount whose status says it is unlimited by
	// design.
	cpu := mustGet(t, m, "rlimit.cpu.single")
	wantLimit(t, cpu, model.UnknownAmount())
	if cpu.Unit != "seconds" || cpu.Status != model.StatusUnlimited {
		t.Errorf("cpu.single = %+v", cpu)
	}

	// An empty units column, and a known limit of zero.
	nice := mustGet(t, m, "rlimit.nice.single")
	wantLimit(t, nice, model.KnownAmount(0))
	if nice.Unit != "" {
		t.Errorf("nice.single unit = %q, want empty", nice.Unit)
	}
}

func TestBasicCaptureCoversEveryKnownRow(t *testing.T) {
	// The capture has all 16 kernel rows; every one must map to its
	// prlimit(1) short key rather than falling into the slug path.
	m := collect(t, fixture(t, "basic"))
	for name, segment := range kernelKeyMap {
		var found bool
		for _, kind := range []string{"single", "soft"} {
			if _, ok := m["rlimit."+segment+"."+kind]; ok {
				found = true
			}
		}
		if !found {
			t.Errorf("no reading for %q under segment %q", name, segment)
		}
	}
	if len(kernelKeyMap) != 16 {
		t.Errorf("kernelKeyMap has %d entries, want 16", len(kernelKeyMap))
	}
}

func TestSoftEqHardCollapsesToOneReading(t *testing.T) {
	m := collect(t, fixture(t, "soft_eq_hard"))
	if _, ok := m["rlimit.nofile.soft"]; ok {
		t.Error("unexpected rlimit.nofile.soft")
	}
	if _, ok := m["rlimit.nofile.hard"]; ok {
		t.Error("unexpected rlimit.nofile.hard")
	}
	wantLimit(t, mustGet(t, m, "rlimit.nofile.single"), model.KnownAmount(1024))
}

func TestUnlimitedMix(t *testing.T) {
	m := collect(t, fixture(t, "unlimited_mix"))
	wantLimit(t, mustGet(t, m, "rlimit.cpu.single"), model.UnknownAmount())
	wantLimit(t, mustGet(t, m, "rlimit.core.soft"), model.KnownAmount(0))
	wantLimit(t, mustGet(t, m, "rlimit.core.hard"), model.UnknownAmount())
	wantLimit(t, mustGet(t, m, "rlimit.nofile.soft"), model.KnownAmount(1024))
	wantLimit(t, mustGet(t, m, "rlimit.nofile.hard"), model.UnknownAmount())
	wantLimit(t, mustGet(t, m, "rlimit.nproc.single"), model.UnknownAmount())
	if got := mustGet(t, m, "rlimit.nice.single").Unit; got != "" {
		t.Errorf("nice unit = %q, want empty", got)
	}
}

func TestUnknownRowKept(t *testing.T) {
	// An invented, unmapped kernel row must still surface a reading: fail
	// open on names, never silently dropped.
	m := collect(t, fixture(t, "unknown_row"))
	soft := mustGet(t, m, "rlimit.flux_capacitors.soft")
	wantLimit(t, soft, model.KnownAmount(10))
	wantLimit(t, mustGet(t, m, "rlimit.flux_capacitors.hard"), model.KnownAmount(20))
	if soft.Unit != "gigawatts" || soft.Label != "flux capacitors" {
		t.Errorf("flux_capacitors.soft = %+v", soft)
	}
}

func TestUnparseableRowDegradesWithoutKillingTheReport(t *testing.T) {
	// One malformed row becomes one reading with StatusUnparseable and
	// every other row still parses.
	m := collect(t, fixture(t, "malformed"))

	bad := mustGet(t, m, "rlimit.unparseable.3")
	if bad.Status != model.StatusUnparseable || bad.Limit.Known {
		t.Errorf("bad row = %+v", bad)
	}
	wantNotes := `unparseable: "this line has no proper column separation at all"`
	if bad.Notes != wantNotes {
		t.Errorf("Notes = %q, want %q", bad.Notes, wantNotes)
	}

	if got := mustGet(t, m, "rlimit.cpu.single").Status; got != model.StatusUnlimited {
		t.Errorf("cpu status = %q", got)
	}
	wantLimit(t, mustGet(t, m, "rlimit.nofile.soft"), model.KnownAmount(1024))
	wantLimit(t, mustGet(t, m, "rlimit.nofile.hard"), model.KnownAmount(1048576))
}

func TestStructuralFailuresAreSourceErrors(t *testing.T) {
	tests := []struct {
		fixture string
		detail  string
	}{
		{"empty_file", "header"},
		{"header_only", "no data rows"},
		{"missing_header", "header"},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			_, err := New().Collect(pid, fixture(t, tc.fixture))
			var se *sources.SourceError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want *sources.SourceError", err)
			}
			if !strings.Contains(err.Error(), tc.detail) {
				t.Errorf("err = %q, want it to mention %q", err, tc.detail)
			}
			if se.Path != "proc/4242/limits" {
				t.Errorf("Path = %q", se.Path)
			}
			if se.Err != nil {
				t.Errorf("a parse failure should not wrap a read error, got %v", se.Err)
			}
		})
	}
}

func TestLeadingWhitespaceRowIsRejected(t *testing.T) {
	// Split on 2+ spaces, an indented row would yield an empty name with
	// real-looking numbers. It must be degraded, not accepted as a
	// nameless-but-numeric reading.
	m := collect(t, fixture(t, "leading_whitespace"))
	if _, ok := m["rlimit.nofile.soft"]; ok {
		t.Error("indented row produced a nofile reading")
	}
	bad := mustGet(t, m, "rlimit.unparseable.3")
	if bad.Status != model.StatusUnparseable || bad.Label != "" {
		t.Errorf("bad row = %+v", bad)
	}
}

func TestTabIndentedRowIsRejected(t *testing.T) {
	m := collect(t, inMemory(header+"\tMax open files            1024                 1048576              files\n"))
	if got := mustGet(t, m, "rlimit.unparseable.2").Status; got != model.StatusUnparseable {
		t.Errorf("status = %q", got)
	}
}

func TestColumnDriftBothSides(t *testing.T) {
	// Verbatim kernel bytes from `prlimit --fsize=18446744073709551614`: the
	// 20-digit value fills its field exactly, so the column separator drops
	// to a single space. This value overflows int64 and exceeds 2^53, so it
	// also pins that limits are exact uint64 values.
	m := collect(t, fixture(t, "drift"))

	drifted := mustGet(t, m, "rlimit.fsize.single")
	wantLimit(t, drifted, model.KnownAmount(18446744073709551614))
	if drifted.Unit != "bytes" || drifted.Status != model.StatusUnmeasured {
		t.Errorf("fsize.single = %+v", drifted)
	}
	if got := mustGet(t, m, "rlimit.cpu.single").Status; got != model.StatusUnlimited {
		t.Errorf("cpu status = %q", got)
	}
	wantLimit(t, mustGet(t, m, "rlimit.nofile.soft"), model.KnownAmount(1024))
}

func TestColumnDriftOneSide(t *testing.T) {
	// Verbatim kernel bytes for `prlimit --fsize=10000000000000000000:unlimited`:
	// only the soft value fills its field, so only the run between soft and
	// hard collapses. The coarse split yields "10000000000000000000
	// unlimited" glued together and must fall back to the single-space split.
	m := collect(t, fixture(t, "single_sided_drift"))

	soft := mustGet(t, m, "rlimit.fsize.soft")
	wantLimit(t, soft, model.KnownAmount(10000000000000000000))
	if soft.Unit != "bytes" {
		t.Errorf("fsize.soft unit = %q", soft.Unit)
	}
	hard := mustGet(t, m, "rlimit.fsize.hard")
	wantLimit(t, hard, model.UnknownAmount())
	if hard.Status != model.StatusUnlimited {
		t.Errorf("fsize.hard status = %q", hard.Status)
	}
	if got := mustGet(t, m, "rlimit.cpu.single").Status; got != model.StatusUnlimited {
		t.Errorf("cpu status = %q", got)
	}
	wantLimit(t, mustGet(t, m, "rlimit.nofile.soft"), model.KnownAmount(1024))
}

func TestColumnDriftWithNoUnit(t *testing.T) {
	// Both-sided drift on a row whose unit column is legitimately empty
	// (like "Max nice priority"): the merged tail has only 2 tokens, not 3,
	// and must still resolve.
	m := collect(t, inMemory(header+
		"Max nice priority         18446744073709551614 18446744073709551613\n"))
	wantLimit(t, mustGet(t, m, "rlimit.nice.soft"), model.KnownAmount(18446744073709551614))
	wantLimit(t, mustGet(t, m, "rlimit.nice.hard"), model.KnownAmount(18446744073709551613))
	if got := mustGet(t, m, "rlimit.nice.soft").Unit; got != "" {
		t.Errorf("unit = %q, want empty", got)
	}
}

func TestMalformedValuesAreRejected(t *testing.T) {
	// strconv.ParseUint accepts a leading "+", and with base 0 underscores,
	// but none of these is ever real kernel output. The row must degrade
	// rather than be accepted.
	for _, bad := range []string{
		"10_240", "+1024", "-5", "0x400", "1e3", "1.5", "unlimited2", "UNLIMITED",
		"\u0661\u0662\u0663",   // Arabic-Indic digits: Unicode digits, but not ASCII
		"18446744073709551616", // 2^64, one past the largest uint64
	} {
		t.Run(bad, func(t *testing.T) {
			m := collect(t, inMemory(header+
				"Max cpu time              unlimited            unlimited            seconds\n"+
				"Max open files            "+bad+"               1048576              files\n"))
			if _, ok := m["rlimit.nofile.soft"]; ok {
				t.Errorf("%q was accepted as a soft limit", bad)
			}
			row := mustGet(t, m, "rlimit.unparseable.3")
			if row.Status != model.StatusUnparseable {
				t.Errorf("status = %q", row.Status)
			}
			// The good row before it is untouched.
			mustGet(t, m, "rlimit.cpu.single")
		})
	}
}

func TestParseValue(t *testing.T) {
	tests := []struct {
		raw    string
		want   model.Amount
		wantOK bool
	}{
		{"unlimited", model.UnknownAmount(), true},
		{"0", model.KnownAmount(0), true},
		{"1024", model.KnownAmount(1024), true},
		{"18446744073709551614", model.KnownAmount(18446744073709551614), true},
		{"18446744073709551615", model.KnownAmount(18446744073709551615), true},
		{"", model.UnknownAmount(), false},
		{"+1", model.UnknownAmount(), false},
		{"-0", model.UnknownAmount(), false},
		{"1_0", model.UnknownAmount(), false},
		{"18446744073709551616", model.UnknownAmount(), false},
	}
	for _, tc := range tests {
		got, ok := parseValue(tc.raw)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("parseValue(%q) = (%+v, %v), want (%+v, %v)", tc.raw, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestCRLFFixtureParsesCleanly(t *testing.T) {
	// The committed crlf fixture must still contain CRs: .gitattributes
	// marks it -text so no checkout rewrites it, and this guard fails
	// loudly rather than letting the test below pass vacuously against a
	// fixture that lost its CRs.
	raw, err := fs.ReadFile(fixture(t, "crlf"), "proc/4242/limits")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("\r\n")) {
		t.Fatal("crlf fixture has no CRLF line endings; it has been rewritten")
	}

	m := collect(t, fixture(t, "crlf"))
	wantLimit(t, mustGet(t, m, "rlimit.nofile.soft"), model.KnownAmount(1024))
	wantLimit(t, mustGet(t, m, "rlimit.nofile.hard"), model.KnownAmount(1048576))
	// No stray "\r" leaking into a unit or a label.
	for _, r := range m {
		if strings.ContainsAny(r.Unit+r.Label+r.Notes+r.Key, "\r\n") {
			t.Errorf("reading %q carries a line terminator: %+v", r.Key, r)
		}
	}
	if got := mustGet(t, m, "rlimit.nofile.hard").Unit; got != "files" {
		t.Errorf("unit = %q, want files", got)
	}
}

func TestCRLFYieldsSameReadingsAsLF(t *testing.T) {
	collectAll := func(fsys fs.FS) []model.LimitReading {
		t.Helper()
		readings, err := New().Collect(pid, fsys)
		if err != nil {
			t.Fatal(err)
		}
		return readings
	}

	// The crlf fixture against its own bytes with the CRs removed.
	crlfRaw, err := fs.ReadFile(fixture(t, "crlf"), "proc/4242/limits")
	if err != nil {
		t.Fatal(err)
	}
	lf := bytes.ReplaceAll(crlfRaw, []byte("\r\n"), []byte("\n"))
	if bytes.Equal(lf, crlfRaw) {
		t.Fatal("crlf fixture has no CRLF line endings; it has been rewritten")
	}
	if got, want := collectAll(fixture(t, "crlf")), collectAll(inMemory(string(lf))); !reflect.DeepEqual(got, want) {
		t.Errorf("crlf fixture readings differ from its LF form:\n got %+v\nwant %+v", got, want)
	}

	// The full 16-row real capture, converted to CRLF in memory.
	basicRaw, err := fs.ReadFile(fixture(t, "basic"), "proc/4242/limits")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(basicRaw, []byte("\r")) {
		t.Fatal("basic fixture unexpectedly contains CRs")
	}
	asCRLF := bytes.ReplaceAll(basicRaw, []byte("\n"), []byte("\r\n"))
	if got, want := collectAll(inMemory(string(asCRLF))), collectAll(fixture(t, "basic")); !reflect.DeepEqual(got, want) {
		t.Errorf("CRLF form of basic differs from basic:\n got %+v\nwant %+v", got, want)
	}
}

func TestMissingTrailingNewline(t *testing.T) {
	m := collect(t, inMemory(strings.TrimSuffix(
		header+"Max open files            1024                 1048576              files\n", "\n")))
	wantLimit(t, mustGet(t, m, "rlimit.nofile.soft"), model.KnownAmount(1024))
}

func TestDuplicateKernelRowNamesGetSuffixedKeys(t *testing.T) {
	// Never seen on a real kernel, but a hand-edited or corrupt file could
	// repeat a row; the later ones must not shadow the first.
	readings, err := New().Collect(pid, inMemory(header+
		"Max open files            1024                 1024                 files\n"+
		"Max open files            2048                 2048                 files\n"+
		"Max open files            4096                 4096                 files\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) != 3 {
		t.Fatalf("got %d readings, want 3 (none silently dropped)", len(readings))
	}
	m := byKey(t, readings)
	wantLimit(t, mustGet(t, m, "rlimit.nofile.single"), model.KnownAmount(1024))
	wantLimit(t, mustGet(t, m, "rlimit.nofile.single.2"), model.KnownAmount(2048))
	wantLimit(t, mustGet(t, m, "rlimit.nofile.single.3"), model.KnownAmount(4096))
}

func TestUnparseableRowsGetDistinctKeys(t *testing.T) {
	m := collect(t, inMemory(header+"garbage one\n\nanother garbage\n"))
	mustGet(t, m, "rlimit.unparseable.2")
	mustGet(t, m, "rlimit.unparseable.4") // blank line 3 is skipped but counted
}

func TestMarkupInjectionFixtureSlugsTheKey(t *testing.T) {
	m := collect(t, fixture(t, "markup_injection"))
	r := mustGet(t, m, "rlimit.weird_red_evil_bold_thing.soft")
	if r.Label != "weird [/red]evil[bold thing" {
		t.Errorf("label = %q", r.Label)
	}
}

func TestKeySegmentFallback(t *testing.T) {
	if got := keySegment("Max ///"); got != "unknown" {
		t.Errorf("keySegment = %q, want unknown", got)
	}
}

func TestUnreadableFile(t *testing.T) {
	// Both a process that exited between the CLI check and the read
	// (not-exist) and a permission error must become a SourceError that
	// still answers errors.Is for the underlying cause.
	tests := []struct {
		name   string
		fsys   fs.FS
		target error
		detail string
	}{
		{"missing", fstest.MapFS{}, fs.ErrNotExist, "unreadable"},
		{"permission", denyFS{}, fs.ErrPermission, "unreadable: permission denied"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			readings, err := New().Collect(pid, tc.fsys)
			if readings != nil {
				t.Errorf("readings = %v, want nil on error", readings)
			}
			var se *sources.SourceError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want *sources.SourceError", err)
			}
			if !errors.Is(err, tc.target) {
				t.Errorf("errors.Is(err, %v) = false", tc.target)
			}
			if !strings.Contains(err.Error(), tc.detail) {
				t.Errorf("err = %q, want it to contain %q", err, tc.detail)
			}
			if !strings.HasPrefix(err.Error(), "proc/4242/limits: ") {
				t.Errorf("err = %q, want it to start with the path", err)
			}
		})
	}
}

// denyFS answers every open with a permission error, as a hidepid=2 /proc
// does for another user's PID. A stub fs.FS keeps this test identical on
// every OS, unlike chmod.
type denyFS struct{}

func (denyFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
}

func TestSourceName(t *testing.T) {
	var s sources.Source = New()
	if s.Name() != "rlimit" {
		t.Errorf("Name() = %q", s.Name())
	}
}
