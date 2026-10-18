package cgroup

import (
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

const procFile = "proc/4242/cgroup"

// fixture returns an fs.FS rooted at the named committed fixture tree. The
// trees are captures from a real cgroup v2 host and hand-built edge cases;
// tests never mock filesystem reads, they point the fs.FS seam at these.
func fixture(t *testing.T, name string) fs.FS {
	t.Helper()
	fsys := os.DirFS("testdata/" + name)
	if _, err := fs.Stat(fsys, procFile); err != nil {
		t.Fatalf("fixture %q: %v", name, err)
	}
	return fsys
}

// inMemory builds an fs.FS holding a proc cgroup file with the given raw
// content, a v2 mount marker and the given extra files (path to content).
func inMemory(procContent string, files map[string]string) fstest.MapFS {
	m := fstest.MapFS{
		procFile:                           {Data: []byte(procContent)},
		"sys/fs/cgroup/cgroup.controllers": {Data: []byte("cpuset cpu io memory pids\n")},
	}
	for name, content := range files {
		m[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
}

// errorFS delegates to a real tree but fails every open whose name ends in
// suffix with err, the way failingFS does in the cli tests.
type errorFS struct {
	fs.FS
	suffix string
	err    error
}

func (e errorFS) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, e.suffix) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: e.err}
	}
	return e.FS.Open(name)
}

// recordingFS fails the test on any open that is not a valid fs path or
// that reaches above the root, and remembers every name it saw.
type recordingFS struct {
	fs.FS
	t     *testing.T
	names []string
}

func (r *recordingFS) Open(name string) (fs.File, error) {
	r.names = append(r.names, name)
	if !fs.ValidPath(name) || strings.HasPrefix(name, "..") {
		r.t.Errorf("source attempted to open %q", name)
	}
	return r.FS.Open(name)
}

func u(v uint64) *uint64 { return &v }

// want is the expected shape of one reading. A nil limit means unknown.
type want struct {
	key      string
	limit    *uint64
	status   model.Status
	unit     string
	contains []string
	absent   []string
}

// check compares readings to wants in order and enforces the invariants
// every reading from this source must satisfy.
func check(t *testing.T, got []model.LimitReading, wants []want) {
	t.Helper()
	var keys []string
	for _, r := range got {
		keys = append(keys, r.Key)
	}
	if len(got) != len(wants) {
		t.Fatalf("got %d readings %v, want %d", len(got), keys, len(wants))
	}
	for i, w := range wants {
		r := got[i]
		if r.Key != w.key {
			t.Errorf("reading %d: key %q, want %q", i, r.Key, w.key)
			continue
		}
		if w.limit == nil {
			if r.Limit.Known {
				t.Errorf("%s: Limit = %+v, want unknown", r.Key, r.Limit)
			}
		} else if r.Limit != model.KnownAmount(*w.limit) {
			t.Errorf("%s: Limit = %+v, want %d", r.Key, r.Limit, *w.limit)
		}
		if r.Status != w.status {
			t.Errorf("%s: Status = %q, want %q (notes: %s)", r.Key, r.Status, w.status, r.Notes)
		}
		if r.Unit != w.unit {
			t.Errorf("%s: Unit = %q, want %q", r.Key, r.Unit, w.unit)
		}
		for _, s := range w.contains {
			if !strings.Contains(r.Notes, s) {
				t.Errorf("%s: notes lack %q:\n%s", r.Key, s, r.Notes)
			}
		}
		for _, s := range w.absent {
			if strings.Contains(r.Notes, s) {
				t.Errorf("%s: notes contain %q:\n%s", r.Key, s, r.Notes)
			}
		}
		// Invariants of every reading: limits only, never ok.
		if r.Kind != model.KindSingle || r.Source != Name || r.Usage.Known || r.Status == model.StatusOK {
			t.Errorf("%s: kind %q source %q usage %+v status %q", r.Key, r.Kind, r.Source, r.Usage, r.Status)
		}
	}
}

func collect(t *testing.T, fsys fs.FS) []model.LimitReading {
	t.Helper()
	got, err := New().Collect(pid, fsys)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return got
}

// wantStructural asserts a *sources.SourceError with the given path and
// detail fragments, and that no readings came back. It is the form used
// where the error itself needs no further inspection; structuralError is
// the same assertion for a caller that wants the error back.
func wantStructural(t *testing.T, fsys fs.FS, path string, detail ...string) {
	t.Helper()
	// The returned error is deliberately dropped; the assertions inside
	// structuralError are the point here.
	_ = structuralError(t, fsys, path, detail...)
}

// structuralError asserts the same thing as wantStructural and returns the
// error, for callers that go on to check the wrapped cause.
func structuralError(t *testing.T, fsys fs.FS, path string, detail ...string) *sources.SourceError {
	t.Helper()
	got, err := New().Collect(pid, fsys)
	var se *sources.SourceError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *sources.SourceError", err, err)
	}
	if got != nil {
		t.Errorf("readings = %v, want none alongside an error", got)
	}
	if se.Path != path {
		t.Errorf("Path = %q, want %q", se.Path, path)
	}
	for _, d := range detail {
		if !strings.Contains(se.Detail, d) {
			t.Errorf("Detail = %q, want it to contain %q", se.Detail, d)
		}
	}
	return se
}

func TestNameAndNew(t *testing.T) {
	if got := New().Name(); got != "cgroup" || Name != "cgroup" {
		t.Errorf("Name = %q", got)
	}
	var _ sources.Source = New()
}

func TestNestedAncestorBinds(t *testing.T) {
	// A real capture: the leaf allows 100 tasks but a slice above it caps
	// the process at 50. This is the case the tool exists for.
	got := collect(t, fixture(t, "nested_ancestor_binds"))
	check(t, got, []want{
		{key: "cgroup.memory.max", limit: u(1073741824), status: model.StatusUnmeasured, unit: "bytes",
			contains: []string{"bound by /le-test/child (sys/fs/cgroup/le-test/child/memory.max)"},
			absent:   []string{"allows"}},
		{key: "cgroup.memory.high", limit: u(1610612736), status: model.StatusUnmeasured, unit: "bytes",
			contains: []string{
				"memory.high throttles allocation and forces reclaim; it does NOT kill",
				"the OOM ceiling is memory.max",
				"bound by /le-test (sys/fs/cgroup/le-test/memory.high)",
				"leaf /le-test/child sets no limit of its own",
			}},
		{key: "cgroup.pids.max", limit: u(50), status: model.StatusUnmeasured, unit: "processes",
			contains: []string{
				"bound by /le-test (sys/fs/cgroup/le-test/pids.max)",
				"leaf /le-test/child allows 100",
			}},
		{key: "cgroup.cpu.max", limit: u(200), status: model.StatusUnmeasured, unit: "millicores",
			contains: []string{
				"bound by /le-test (sys/fs/cgroup/le-test/cpu.max)",
				`raw cpu.max "20000 100000" (quota/period microseconds) at /le-test`,
			}},
	})
	if got[2].Limit.Value == 100 {
		t.Error("the leaf value 100 was reported instead of the ancestor minimum 50")
	}
}

func TestMemoryHighIsSeparateFromMemoryMax(t *testing.T) {
	got := collect(t, fixture(t, "nested_ancestor_binds"))
	byKey := map[string]model.LimitReading{}
	for _, r := range got {
		byKey[r.Key] = r
	}
	high, hard := byKey["cgroup.memory.high"], byKey["cgroup.memory.max"]
	if high.Key == "" || hard.Key == "" || high.Key == hard.Key {
		t.Fatalf("memory.high and memory.max must be distinct readings: %v", got)
	}
	if high.Limit == hard.Limit {
		t.Errorf("fixture should give the two different limits, both %+v", high.Limit)
	}
	if !strings.HasPrefix(high.Notes, "memory.high throttles") {
		t.Errorf("the throttle clause must come first: %s", high.Notes)
	}
	if strings.Contains(hard.Notes, "throttles") {
		t.Errorf("memory.max notes carry the memory.high clause: %s", hard.Notes)
	}
}

func TestMissingControllerFile(t *testing.T) {
	// A real capture, with an "@" in the cgroup path, where two levels have
	// no cpu.max.
	const leaf = "/user.slice/user-1000.slice/user@1000.service"
	got := collect(t, fixture(t, "missing_controller"))
	check(t, got, []want{
		{key: "cgroup.memory.max", status: model.StatusUnlimited, unit: "bytes",
			contains: []string{"no level imposes a ceiling (leaf " + leaf + ")"}},
		{key: "cgroup.memory.high", status: model.StatusUnlimited, unit: "bytes",
			contains: []string{"memory.high throttles"}},
		{key: "cgroup.pids.max", limit: u(41153), status: model.StatusUnmeasured, unit: "processes",
			contains: []string{
				"bound by /user.slice/user-1000.slice (sys/fs/cgroup/user.slice/user-1000.slice/pids.max)",
				"leaf " + leaf + " sets no limit of its own",
			}},
		{key: "cgroup.cpu.max", status: model.StatusUnlimited, unit: "millicores",
			contains: []string{
				"cpu.max is absent at " + leaf + ", /user.slice/user-1000.slice (normal where the controller is not enabled)",
				`raw cpu.max "max 100000"`,
			}},
	})
}

func TestAllMax(t *testing.T) {
	got := collect(t, fixture(t, "all_max"))
	check(t, got, []want{
		{key: "cgroup.memory.max", status: model.StatusUnlimited, unit: "bytes"},
		{key: "cgroup.memory.high", status: model.StatusUnlimited, unit: "bytes"},
		{key: "cgroup.pids.max", status: model.StatusUnlimited, unit: "processes"},
		{key: "cgroup.cpu.max", status: model.StatusUnlimited, unit: "millicores"},
	})
}

func TestRootLeaf(t *testing.T) {
	got := collect(t, fixture(t, "root_leaf"))
	check(t, got, []want{
		{key: "cgroup.memory.max", status: model.StatusUnlimited, unit: "bytes",
			contains: []string{"leaf /)"}},
		{key: "cgroup.pids.max", limit: u(200), status: model.StatusUnmeasured, unit: "processes",
			contains: []string{"bound by / (sys/fs/cgroup/pids.max)"}},
	})
}

func TestSpacesInPath(t *testing.T) {
	got := collect(t, fixture(t, "spaces_in_path"))
	check(t, got, []want{
		{key: "cgroup.pids.max", limit: u(11), status: model.StatusUnmeasured, unit: "processes",
			contains: []string{
				"bound by /odd name (sys/fs/cgroup/odd name/pids.max)",
				"leaf /odd name/child one allows 99",
			}},
	})
}

func TestDeepChain(t *testing.T) {
	fsys := fixture(t, "deep_chain")
	raw, err := fs.ReadFile(fsys, procFile)
	if err != nil {
		t.Fatal(err)
	}
	if levels := strings.Count(string(raw), "/"); levels < 12 {
		t.Fatalf("fixture has %d levels, want at least 12", levels)
	}
	got := collect(t, fsys)
	check(t, got, []want{
		{key: "cgroup.pids.max", limit: u(30), status: model.StatusUnmeasured, unit: "processes",
			contains: []string{
				"bound by /l01/l02/l03 (sys/fs/cgroup/l01/l02/l03/pids.max)",
				"leaf /l01/l02/l03/l04/l05/l06/l07/l08/l09/l10/l11/l12/l13/l14 allows 1014",
			}},
	})
}

func TestAbsentEverywhereEmitsNoReading(t *testing.T) {
	got := collect(t, fixture(t, "absent_everywhere"))
	check(t, got, []want{
		{key: "cgroup.memory.max", limit: u(4096), status: model.StatusUnmeasured, unit: "bytes"},
	})
}

func TestUnparseableValueDegradesOneReading(t *testing.T) {
	got := collect(t, fixture(t, "unparseable_value"))
	check(t, got, []want{
		{key: "cgroup.memory.max", status: model.StatusUnparseable, unit: "bytes",
			contains: []string{`unparseable at /a/b (sys/fs/cgroup/a/b/memory.max): "abc"`},
			absent:   []string{"1024"}},
		{key: "cgroup.pids.max", limit: u(64), status: model.StatusUnmeasured, unit: "processes"},
	})
}

func TestHugeValuesSurviveAsUint64(t *testing.T) {
	got := collect(t, fixture(t, "huge_values"))
	check(t, got, []want{
		{key: "cgroup.memory.max", limit: u(18446744073709551614), status: model.StatusUnmeasured, unit: "bytes",
			contains: []string{"bound by /a/b"}},
	})
}

func TestOverflowAndPlusPrefixAreUnparseable(t *testing.T) {
	check(t, collect(t, fixture(t, "overflow_value")), []want{
		{key: "cgroup.memory.max", status: model.StatusUnparseable, unit: "bytes",
			contains: []string{`"18446744073709551616"`}},
	})
	check(t, collect(t, fixture(t, "plus_prefixed")), []want{
		{key: "cgroup.pids.max", status: model.StatusUnparseable, unit: "processes",
			contains: []string{`"+50"`}, absent: []string{"bound by"}},
	})
}

func TestUnreadableControllerIsNotUnlimited(t *testing.T) {
	// memory.max at the leaf is a directory in this tree, so reading it
	// fails with something other than "does not exist". The root says "max",
	// which must NOT win: the unreadable level could be the binding one.
	got := collect(t, fixture(t, "unreadable_controller"))
	check(t, got, []want{
		{key: "cgroup.memory.max", status: model.StatusUnreadable, unit: "bytes",
			contains: []string{"unreadable at /a (sys/fs/cgroup/a/memory.max)"}},
		{key: "cgroup.pids.max", limit: u(10), status: model.StatusUnmeasured, unit: "processes"},
	})
	_, err := fs.ReadFile(fixture(t, "unreadable_controller"), "sys/fs/cgroup/a/memory.max")
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("reading a directory gave %v, want a non-ErrNotExist error on this platform", err)
	}
}

func TestPermissionDeniedOnOneControllerDegradesOneReading(t *testing.T) {
	fsys := errorFS{fixture(t, "nested_ancestor_binds"), "/pids.max", fs.ErrPermission}
	got := collect(t, fsys)
	check(t, got, []want{
		{key: "cgroup.memory.max", limit: u(1073741824), status: model.StatusUnmeasured, unit: "bytes"},
		{key: "cgroup.memory.high", limit: u(1610612736), status: model.StatusUnmeasured, unit: "bytes"},
		{key: "cgroup.pids.max", status: model.StatusUnreadable, unit: "processes",
			contains: []string{"permission denied", "limit withheld"}},
		{key: "cgroup.cpu.max", limit: u(200), status: model.StatusUnmeasured, unit: "millicores"},
	})
}

func TestUnparseableOutranksUnreadable(t *testing.T) {
	inner := inMemory("0::/a/b\n", map[string]string{
		"sys/fs/cgroup/a/b/memory.max": "5\n",
		"sys/fs/cgroup/a/memory.max":   "abc\n",
		"sys/fs/cgroup/memory.max":     "7\n",
	})
	// Leaf unreadable, middle unparseable, root finite: the ladder says
	// unparseable wins, and both offenders are named.
	got := collect(t, errorFS{inner, "/a/b/memory.max", fs.ErrPermission})
	check(t, got, []want{
		{key: "cgroup.memory.max", status: model.StatusUnparseable, unit: "bytes",
			contains: []string{"unparseable at /a", "unreadable at /a/b"}, absent: []string{"bound by"}},
	})
}

func TestTieReportsLeafMostLevel(t *testing.T) {
	fsys := inMemory("0::/a/b\n", map[string]string{
		"sys/fs/cgroup/a/b/pids.max": "10\n",
		"sys/fs/cgroup/a/pids.max":   "10\n",
		"sys/fs/cgroup/pids.max":     "10\n",
	})
	check(t, collect(t, fsys), []want{
		{key: "cgroup.pids.max", limit: u(10), status: model.StatusUnmeasured, unit: "processes",
			contains: []string{"bound by /a/b (sys/fs/cgroup/a/b/pids.max)"}, absent: []string{"allows"}},
	})
}

func TestCPUVariants(t *testing.T) {
	tests := []struct {
		content string
		limit   *uint64
		status  model.Status
		note    string
	}{
		{"max 100000", nil, model.StatusUnlimited, `raw cpu.max "max 100000"`},
		{"max abc", nil, model.StatusUnparseable, `"max abc"`}, // a corrupt period must not assert "no ceiling"
		{"max 0", nil, model.StatusUnparseable, `"max 0"`},
		{"50000 100000", u(500), model.StatusUnmeasured, `raw cpu.max "50000 100000"`},
		{"20000 100000", u(200), model.StatusUnmeasured, ""},
		{"0 100000", u(0), model.StatusUnmeasured, `raw cpu.max "0 100000"`},
		{"100000 0", nil, model.StatusUnparseable, `"100000 0"`},
		{"50000", nil, model.StatusUnparseable, `"50000"`},
		{"50000 100000 7", nil, model.StatusUnparseable, `"50000 100000 7"`},
		{"max", nil, model.StatusUnparseable, `"max"`},
		{"", nil, model.StatusUnparseable, `""`},
		{"+5 100", nil, model.StatusUnparseable, `"+5 100"`},
		{"5 1_0", nil, model.StatusUnparseable, `"5 1_0"`},
		{"9223372036854775808 1", nil, model.StatusUnparseable, "do not fit in 64 bits"},
		{"9223372036854775807 1", nil, model.StatusUnparseable, "do not fit in 64 bits"},
		{"18446744073709551616 100", nil, model.StatusUnparseable, `"18446744073709551616 100"`},
		{"9223372036854775807 100000", u(92233720368547758), model.StatusUnmeasured, ""},
		{"18446744073709551615 18446744073709551615", u(1000), model.StatusUnmeasured, ""},
		{"18446744073709551615 1000", u(18446744073709551615), model.StatusUnmeasured, ""},
		{"18446744073709551615 999", nil, model.StatusUnparseable, "do not fit in 64 bits"},
	}
	for _, tc := range tests {
		t.Run(tc.content, func(t *testing.T) {
			fsys := inMemory("0::/a\n", map[string]string{"sys/fs/cgroup/a/cpu.max": tc.content + "\n"})
			got := collect(t, fsys)
			w := want{key: "cgroup.cpu.max", limit: tc.limit, status: tc.status, unit: "millicores"}
			if tc.note != "" {
				w.contains = []string{tc.note}
			}
			check(t, got, []want{w})
		})
	}
}

func TestUint64Boundaries(t *testing.T) {
	tests := []struct {
		content string
		limit   *uint64
		status  model.Status
	}{
		{"0", u(0), model.StatusUnmeasured},
		{"18446744073709551614", u(18446744073709551614), model.StatusUnmeasured},
		// "max" is the only unlimited marker, so 2^64-1 is an ordinary value.
		{"18446744073709551615", u(18446744073709551615), model.StatusUnmeasured},
		{"18446744073709551616", nil, model.StatusUnparseable},
		{"9223372036854775808", u(9223372036854775808), model.StatusUnmeasured}, // 2^63, overflows int64
		{"", nil, model.StatusUnparseable},
		{"-1", nil, model.StatusUnparseable},
		{"1_0", nil, model.StatusUnparseable},
		{"0x10", nil, model.StatusUnparseable},
		{"1.5", nil, model.StatusUnparseable},
		{"MAX", nil, model.StatusUnparseable},
		{"max 1", nil, model.StatusUnparseable},
	}
	for _, tc := range tests {
		t.Run(tc.content, func(t *testing.T) {
			fsys := inMemory("0::/a\n", map[string]string{"sys/fs/cgroup/a/pids.max": tc.content + "\n"})
			check(t, collect(t, fsys), []want{{key: "cgroup.pids.max", limit: tc.limit, status: tc.status, unit: "processes"}})
		})
	}
}

func TestSurroundingWhitespaceAndCRLFInControllerFile(t *testing.T) {
	fsys := inMemory("0::/a\n", map[string]string{
		"sys/fs/cgroup/a/pids.max": "  50\r\n",
		"sys/fs/cgroup/a/cpu.max":  "20000 100000\r\n",
	})
	check(t, collect(t, fsys), []want{
		{key: "cgroup.pids.max", limit: u(50), status: model.StatusUnmeasured, unit: "processes"},
		{key: "cgroup.cpu.max", limit: u(200), status: model.StatusUnmeasured, unit: "millicores"},
	})
}

func TestUnparseableNoteIsBounded(t *testing.T) {
	blob := strings.Repeat("x", 5000)
	fsys := inMemory("0::/a\n", map[string]string{"sys/fs/cgroup/a/pids.max": blob})
	got := collect(t, fsys)
	check(t, got, []want{{key: "cgroup.pids.max", status: model.StatusUnparseable, unit: "processes"}})
	if len(got[0].Notes) > 400 {
		t.Errorf("note is %d bytes, want it bounded", len(got[0].Notes))
	}
	if !strings.Contains(got[0].Notes, strings.Repeat("x", 64)+`"...`) || strings.Contains(got[0].Notes, strings.Repeat("x", 65)) {
		t.Errorf("note is not truncated to 64 bytes: %s", got[0].Notes)
	}
}

func TestProcFileVariants(t *testing.T) {
	pids := map[string]string{"sys/fs/cgroup/a/pids.max": "3\n"}
	pidsWant := []want{{key: "cgroup.pids.max", limit: u(3), status: model.StatusUnmeasured, unit: "processes"}}

	t.Run("CRLF is tolerated", func(t *testing.T) {
		check(t, collect(t, inMemory("0::/a\r\n", pids)), pidsWant)
	})
	t.Run("blank lines around the entry", func(t *testing.T) {
		check(t, collect(t, inMemory("\n  \n0::/a\n\n", pids)), pidsWant)
	})
	t.Run("no trailing newline", func(t *testing.T) {
		check(t, collect(t, inMemory("0::/a", pids)), pidsWant)
	})
	t.Run("colon in the cgroup path", func(t *testing.T) {
		// SplitN with a limit of 3 keeps everything after the second colon.
		fsys := inMemory("0::/odd:name\n", map[string]string{"sys/fs/cgroup/odd:name/pids.max": "4\n"})
		check(t, collect(t, fsys), []want{{
			key: "cgroup.pids.max", limit: u(4), status: model.StatusUnmeasured, unit: "processes",
			contains: []string{"bound by /odd:name"}}})
	})
	t.Run("repeated and trailing slashes", func(t *testing.T) {
		check(t, collect(t, inMemory("0::/a//\n", pids)), pidsWant)
	})
	t.Run("name with dots, dashes, at and spaces", func(t *testing.T) {
		fsys := inMemory("0::/a.b-c@d e\n", map[string]string{"sys/fs/cgroup/a.b-c@d e/pids.max": "4\n"})
		check(t, collect(t, fsys), []want{{key: "cgroup.pids.max", limit: u(4), status: model.StatusUnmeasured, unit: "processes"}})
	})
	t.Run("a path containing a newline fails closed", func(t *testing.T) {
		// "/weird" must not be silently resolved from the first half.
		fsys := inMemory("0::/weird\nname\n", map[string]string{"sys/fs/cgroup/weird/pids.max": "1\n"})
		wantStructural(t, fsys, procFile, "malformed entry", `"name"`)
	})
}

func TestStructuralErrors(t *testing.T) {
	t.Run("missing proc file", func(t *testing.T) {
		fsys := fstest.MapFS{"sys/fs/cgroup/cgroup.controllers": {Data: []byte("x")}}
		se := structuralError(t, fsys, procFile, "unreadable")
		if !errors.Is(se, fs.ErrNotExist) {
			t.Errorf("errors.Is(ErrNotExist) = false for %v", se)
		}
	})
	t.Run("empty file", func(t *testing.T) {
		wantStructural(t, inMemory("", nil), procFile, "empty: no cgroup entries")
	})
	t.Run("blank lines only", func(t *testing.T) {
		wantStructural(t, inMemory("\n \r\n\n", nil), procFile, "empty: no cgroup entries")
	})
	t.Run("too few colons", func(t *testing.T) {
		wantStructural(t, inMemory("0:/a\n", nil), procFile, "malformed entry", `"0:/a"`)
	})
	t.Run("two unified entries", func(t *testing.T) {
		wantStructural(t, inMemory("0::/a\n0::/b\n", nil), procFile, "2 unified cgroup entries")
	})
	t.Run("relative path", func(t *testing.T) {
		wantStructural(t, inMemory("0::relative\n", nil), procFile, "not absolute", `"relative"`)
	})
	t.Run("dot-dot component", func(t *testing.T) {
		wantStructural(t, inMemory("0::/a/../b\n", nil), procFile, `".."`, "refused rather than walked")
	})
	t.Run("dot component", func(t *testing.T) {
		wantStructural(t, inMemory("0::/a/./b\n", nil), procFile, `"."`, "refused rather than walked")
	})
	t.Run("hierarchy ID without a controller is not unified", func(t *testing.T) {
		wantStructural(t, inMemory("5::/a\n", nil), procFile, "cgroup v1 hierarchy")
	})
	t.Run("controller list without hierarchy ID zero is not unified", func(t *testing.T) {
		wantStructural(t, inMemory("0:pids:/a\n", nil), procFile, "cgroup v1 hierarchy")
	})
	t.Run("permission denied on the proc file", func(t *testing.T) {
		se := structuralError(t, errorFS{fixture(t, "all_max"), "/cgroup", fs.ErrPermission}, procFile, "permission denied")
		if !errors.Is(se, fs.ErrPermission) || errors.Is(se, fs.ErrNotExist) {
			t.Errorf("Unwrap chain lost the permission error: %v", se.Err)
		}
	})
}

func TestV1IsReportedAndNeverRead(t *testing.T) {
	// The tree carries decoy v1 numbers (999 and 7) that must never surface.
	se := structuralError(t, fixture(t, "v1"), procFile,
		"cgroup v1 hierarchy", `"pids"`, `"memory"`, `"cpu,cpuacct"`, `"name=systemd"`,
		"cgroup v2 unified hierarchy only", "will not report v1 numbers")
	if strings.Contains(se.Detail, "hybrid") {
		t.Errorf("a pure v1 host was called hybrid: %s", se.Detail)
	}
	if strings.Contains(se.Detail, "999") {
		t.Errorf("decoy value leaked into the error: %s", se.Detail)
	}
	if se.Err != nil {
		t.Errorf("a structural v1 failure must not wrap a read error: %v", se.Err)
	}
	// The v1 controllers come out in file order.
	if i, j := strings.Index(se.Detail, `"pids"`), strings.Index(se.Detail, `"memory"`); i > j {
		t.Errorf("controllers not in file order: %s", se.Detail)
	}
}

func TestHybridIsReported(t *testing.T) {
	se := structuralError(t, fixture(t, "hybrid"), procFile,
		"hybrid cgroup hierarchy", `"name=systemd"`, "will not report v1 numbers")
	if strings.Contains(se.Detail, "cgroup v1 hierarchy:") {
		t.Errorf("a hybrid host was called pure v1: %s", se.Detail)
	}
}

func TestMountAndLeafErrors(t *testing.T) {
	t.Run("no v2 mount", func(t *testing.T) {
		se := structuralError(t, fixture(t, "no_v2_mount"), "sys/fs/cgroup/cgroup.controllers",
			"not mounted at sys/fs/cgroup", "exists only on cgroup v2")
		if !errors.Is(se, fs.ErrNotExist) {
			t.Errorf("errors.Is(ErrNotExist) = false for %v", se)
		}
	})
	t.Run("mount marker unreadable", func(t *testing.T) {
		fsys := errorFS{fixture(t, "all_max"), "/cgroup.controllers", fs.ErrPermission}
		se := structuralError(t, fsys, "sys/fs/cgroup/cgroup.controllers", "unreadable", "permission denied")
		if !errors.Is(se, fs.ErrPermission) {
			t.Errorf("Unwrap chain lost the permission error: %v", se.Err)
		}
	})
	t.Run("leaf directory missing", func(t *testing.T) {
		se := structuralError(t, fixture(t, "leaf_dir_missing"), "sys/fs/cgroup/le-test/child",
			"not present under the unified mount", "container cgroup namespace", "process exited")
		if !errors.Is(se, fs.ErrNotExist) {
			t.Errorf("errors.Is(ErrNotExist) = false for %v", se)
		}
	})
	t.Run("leaf path is a file", func(t *testing.T) {
		fsys := inMemory("0::/a\n", map[string]string{"sys/fs/cgroup/a": "not a directory"})
		se := structuralError(t, fsys, "sys/fs/cgroup/a", "not present under the unified mount")
		if se.Err != nil {
			t.Errorf("a non-directory leaf is structural, not a read failure: %v", se.Err)
		}
	})
	t.Run("leaf directory unreadable", func(t *testing.T) {
		fsys := errorFS{fixture(t, "all_max"), "sys/fs/cgroup/system.slice", fs.ErrPermission}
		se := structuralError(t, fsys, "sys/fs/cgroup/system.slice", "unreadable", "permission denied")
		if !errors.Is(se, fs.ErrPermission) {
			t.Errorf("Unwrap chain lost the permission error: %v", se.Err)
		}
	})
}

func TestTraversalIsRefusedBeforeAnyCgroupRead(t *testing.T) {
	rec := &recordingFS{FS: inMemory("0::/a/../../etc\n", nil), t: t}
	wantStructural(t, rec, procFile, `".."`)
	for _, name := range rec.names {
		if name != procFile {
			t.Errorf("opened %q after refusing the path", name)
		}
	}
}

func TestEveryOpenIsAValidRelativePath(t *testing.T) {
	// A full successful walk over awkward names must stay inside the root.
	for _, name := range []string{"nested_ancestor_binds", "spaces_in_path", "missing_controller", "deep_chain"} {
		rec := &recordingFS{FS: fixture(t, name), t: t}
		collect(t, rec)
		if len(rec.names) == 0 {
			t.Fatalf("%s: recorded nothing", name)
		}
	}
}

func TestDeterministicAndOrdered(t *testing.T) {
	fsys := fixture(t, "nested_ancestor_binds")
	first, second := collect(t, fsys), collect(t, fsys)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two collections differ:\n%v\n%v", first, second)
	}
	var keys []string
	for _, r := range first {
		keys = append(keys, r.Key)
	}
	wantKeys := []string{"cgroup.memory.max", "cgroup.memory.high", "cgroup.pids.max", "cgroup.cpu.max"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Errorf("order = %v, want %v", keys, wantKeys)
	}
	labels := []string{"memory", "memory (high)", "tasks", "cpu bandwidth"}
	for i, r := range first {
		if r.Label != labels[i] {
			t.Errorf("%s: label %q, want %q", r.Key, r.Label, labels[i])
		}
	}
}

func TestBuildChain(t *testing.T) {
	chain, err := buildChain(procFile, "/a/b c")
	if err != nil {
		t.Fatal(err)
	}
	want := []level{
		{"/a/b c", "sys/fs/cgroup/a/b c"},
		{"/a", "sys/fs/cgroup/a"},
		{"/", "sys/fs/cgroup"},
	}
	if !reflect.DeepEqual(chain, want) {
		t.Errorf("chain = %v, want %v", chain, want)
	}
	root, err := buildChain(procFile, "/")
	if err != nil || len(root) != 1 || root[0].display != "/" {
		t.Errorf("root chain = %v, %v", root, err)
	}
}

func TestLeafPathAppearsInEveryNote(t *testing.T) {
	// A JSON consumer must see which cgroup was resolved without a second
	// call, whatever the reading's status.
	leaves := map[string]string{
		"nested_ancestor_binds": "/le-test/child",
		"missing_controller":    "/user.slice/user-1000.slice/user@1000.service",
		"all_max":               "/system.slice",
		"unparseable_value":     "/a/b",
		"unreadable_controller": "/a",
		"spaces_in_path":        "/odd name/child one",
		"huge_values":           "/a/b",
		"overflow_value":        "/a",
		"plus_prefixed":         "/a",
		"absent_everywhere":     "/a",
	}
	for name, leaf := range leaves {
		for _, r := range collect(t, fixture(t, name)) {
			if !strings.Contains(r.Notes, leaf) {
				t.Errorf("%s %s: notes lack leaf %q:\n%s", name, r.Key, leaf, r.Notes)
			}
		}
	}
}
