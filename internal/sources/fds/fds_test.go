package fds

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mabunemeh/limits-explain/internal/model"
)

const pid = 4242

func fixture(t *testing.T, name string) fs.FS {
	t.Helper()
	root := filepath.Join("testdata", name)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("no such fixture tree: %s (%v)", root, err)
	}
	return os.DirFS(root)
}

func row(key string, kind model.Kind, limit model.Amount) model.LimitReading {
	return model.LimitReading{
		Key: key, Kind: kind, Label: key, Source: "rlimit", Limit: limit,
		Usage: model.UnknownAmount(), Status: model.DefaultStatus(limit, model.UnknownAmount()),
	}
}

// rlimitRows is soft, hard, nproc soft, nproc hard, cgroup pids.max.
func rlimitRows(soft, hard uint64) []model.LimitReading {
	return []model.LimitReading{
		row("rlimit.nofile.soft", model.KindSoft, model.KnownAmount(soft)),
		row("rlimit.nofile.hard", model.KindHard, model.KnownAmount(hard)),
		row("rlimit.nproc.soft", model.KindSoft, model.KnownAmount(100)),
		row("rlimit.nproc.hard", model.KindHard, model.KnownAmount(100)),
		row("cgroup.pids.max", model.KindSingle, model.KnownAmount(50)),
	}
}

func byKey(rows []model.LimitReading, key string) model.LimitReading {
	for _, r := range rows {
		if r.Key == key {
			return r
		}
	}
	return model.LimitReading{}
}

func mapFS(entries ...string) fstest.MapFS {
	m := fstest.MapFS{}
	for _, e := range entries {
		m["proc/4242/fd/"+e] = &fstest.MapFile{}
	}
	return m
}

// errFS fails every open with a fixed error, as a foreign fd directory does.
type errFS struct{ err error }

func (e errFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: e.err}
}

func TestNofileUsage(t *testing.T) {
	tests := []struct {
		name      string
		fsys      fs.FS
		soft      uint64
		hard      uint64
		wantUsage uint64
		wantNote  string
		wantNone  bool // usage must stay unknown
	}{
		{name: "normal", fsys: fixture(t, "normal"), soft: 100, hard: 200, wantUsage: 5},
		{name: "count equals soft", fsys: fixture(t, "at_soft"), soft: 8, hard: 16, wantUsage: 8},
		{name: "fd above lowered soft", fsys: fixture(t, "over_soft"), soft: 8, hard: 16, wantUsage: 4,
			wantNote: "1 open descriptor is numbered at or above the soft limit"},
		{name: "fd number equal to soft", fsys: mapFS("0", "8"), soft: 8, hard: 16, wantUsage: 2,
			wantNote: "1 open descriptor is numbered at or above the soft limit"},
		{name: "several above soft", fsys: mapFS("0", "9", "10"), soft: 8, hard: 16, wantUsage: 3,
			wantNote: "2 open descriptors are numbered at or above the soft limit"},
		{name: "non-digit entry", fsys: fixture(t, "nondigit"), soft: 100, hard: 200, wantNone: true,
			wantNote: "unexpected entry"},
		{name: "signed entry", fsys: mapFS("0", "+1"), soft: 100, hard: 200, wantNone: true,
			wantNote: "unexpected entry"},
		{name: "non-ascii digit", fsys: mapFS("0", string(rune(0x663))), soft: 100, hard: 200, wantNone: true,
			wantNote: "unexpected entry"},
		{name: "overflowing number", fsys: mapFS("0", "99999999999999999999999"), soft: 100, hard: 200,
			wantNone: true, wantNote: "unexpected entry"},
		{name: "empty dir", fsys: fstest.MapFS{"proc/4242/fd": &fstest.MapFile{Mode: fs.ModeDir}},
			soft: 100, hard: 200, wantUsage: 0},
		{name: "no fd dir", fsys: fstest.MapFS{}, soft: 100, hard: 200, wantNone: true,
			wantNote: "fd count unreadable: file does not exist"},
		{name: "permission denied", fsys: errFS{fs.ErrPermission}, soft: 100, hard: 200, wantNone: true,
			wantNote: "fd count unreadable: permission denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Apply(pid, tt.fsys, rlimitRows(tt.soft, tt.hard))
			for _, r := range got[:2] {
				if tt.wantNone {
					if r.Usage.Known || r.Status != model.StatusUnmeasured {
						t.Errorf("%s = %+v, want unmeasured", r.Key, r)
					}
				} else if r.Usage != model.KnownAmount(tt.wantUsage) || r.Status != model.StatusOK {
					t.Errorf("%s usage/status = %+v/%s, want %d/ok", r.Key, r.Usage, r.Status, tt.wantUsage)
				}
				if tt.wantNote == "" && r.Notes != "" {
					t.Errorf("%s notes = %q, want none", r.Key, r.Notes)
				}
				if !strings.Contains(r.Notes, tt.wantNote) {
					t.Errorf("%s notes = %q, want %q", r.Key, r.Notes, tt.wantNote)
				}
			}
		})
	}
}

func TestFullyConsumedAtCount(t *testing.T) {
	got := Apply(pid, fixture(t, "at_soft"), rlimitRows(8, 16))
	if pct, _ := got[0].Percent(); pct != "100.0" {
		t.Errorf("soft percent = %s, want 100.0", pct)
	}
	if !got[0].AtOrAbove(90) {
		t.Error("count == soft limit must be at or above 90")
	}
	if got[1].AtOrAbove(90) {
		t.Error("hard row at 8 of 16 must not trip 90")
	}
}

func TestSingleRowAndUnlimitedAndFailedRowsLeftAlone(t *testing.T) {
	unlimited := row("rlimit.nofile.hard", model.KindHard, model.UnknownAmount())
	unlimited.Notes = "kept"
	unparseable := row("rlimit.nofile.soft", model.KindSoft, model.UnknownAmount())
	unparseable.Status = model.StatusUnparseable
	in := []model.LimitReading{unlimited, unparseable}
	got := Apply(pid, fixture(t, "normal"), in)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("rows changed:\n got %+v\nwant %+v", got, in)
	}

	single := []model.LimitReading{row("rlimit.nofile.single", model.KindSingle, model.KnownAmount(10))}
	got = Apply(pid, fixture(t, "normal"), single)
	if got[0].Usage != model.KnownAmount(5) || got[0].Status != model.StatusOK {
		t.Errorf("single row = %+v", got[0])
	}

	// An unlimited soft row with a finite hard row: usage is filled on the
	// hard row and nothing is compared against a soft limit that is absent.
	mixed := []model.LimitReading{
		row("rlimit.nofile.soft", model.KindSoft, model.UnknownAmount()),
		row("rlimit.nofile.hard", model.KindHard, model.KnownAmount(10)),
	}
	got = Apply(pid, fixture(t, "over_soft"), mixed)
	if got[0].Usage.Known || got[0].Status != model.StatusUnlimited || got[0].Notes != "" {
		t.Errorf("unlimited soft row = %+v", got[0])
	}
	if got[1].Usage != model.KnownAmount(4) || got[1].Notes != "" {
		t.Errorf("hard row = %+v", got[1])
	}
}

func status(s string) fstest.MapFS {
	return fstest.MapFS{"proc/4242/status": &fstest.MapFile{Data: []byte(s)}}
}

func TestNprocAndPidsNotes(t *testing.T) {
	tests := []struct {
		name     string
		fsys     fs.FS
		wantNpr  []string
		wantUID0 bool
		wantPids string // "" means no note on pids.max
	}{
		{name: "user", fsys: fixture(t, "normal"),
			wantNpr: []string{
				"per-user limit: counts every task of real uid 1000 system-wide, not just this process",
				"this process has 4 threads"},
			wantPids: "this process has 4 threads"},
		{name: "uid 0", fsys: fixture(t, "uid0"),
			wantNpr:  []string{"real uid 0", "this process has 2 threads", "not enforced for uid 0"},
			wantUID0: true, wantPids: "this process has 2 threads"},
		{name: "missing status", fsys: fixture(t, "no_status"),
			wantNpr: []string{"the real uid", "process status unreadable: "}},
		{name: "garbled", fsys: fixture(t, "garbled"),
			wantNpr: []string{"the real uid", "thread count unreadable"}},
		{name: "one thread", fsys: status("Uid:\t7\t7\t7\t7\nThreads:\t1\n"),
			wantNpr: []string{"real uid 7", "this process has 1 thread"}, wantPids: "this process has 1 thread"},
		{name: "uid with three fields", fsys: status("Uid:\t0\t0\t0\nThreads:\t3\n"),
			wantNpr: []string{"the real uid", "this process has 3 threads"}, wantPids: "this process has 3 threads"},
		{name: "signed threads", fsys: status("Uid:\t5\t5\t5\t5\nThreads:\t+3\n"),
			wantNpr: []string{"real uid 5", "thread count unreadable"}},
		{name: "first line wins", fsys: status("Uid:\t5\t5\t5\t5\nUid:\t0\t0\t0\t0\nThreads:\t2\nThreads:\t9\n"),
			wantNpr: []string{"real uid 5", "this process has 2 threads"}, wantPids: "this process has 2 threads"},
		{name: "real uid not effective", fsys: status("Uid:\t5\t6\t7\t8\nThreads:\t2\n"),
			wantNpr: []string{"real uid 5", "this process has 2 threads"}, wantPids: "this process has 2 threads"},
		{name: "crlf", fsys: status("Uid:\t5\t5\t5\t5\r\nThreads:\t2\r\n"),
			wantNpr: []string{"real uid 5", "this process has 2 threads"}, wantPids: "this process has 2 threads"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Apply(pid, tt.fsys, rlimitRows(100, 200))
			for _, key := range []string{"rlimit.nproc.soft", "rlimit.nproc.hard"} {
				r := byKey(got, key)
				if r.Usage.Known || r.Status != model.StatusUnmeasured {
					t.Errorf("%s must stay unmeasured: %+v", key, r)
				}
				for _, w := range tt.wantNpr {
					if !strings.Contains(r.Notes, w) {
						t.Errorf("%s notes = %q, want %q", key, r.Notes, w)
					}
				}
				if has := strings.Contains(r.Notes, "not enforced for uid 0"); has != tt.wantUID0 {
					t.Errorf("%s notes = %q: uid 0 wording present = %v, want %v", key, r.Notes, has, tt.wantUID0)
				}
			}
			if p := byKey(got, "cgroup.pids.max"); p.Notes != tt.wantPids {
				t.Errorf("pids.max notes = %q, want %q", p.Notes, tt.wantPids)
			}
		})
	}
}

func TestNotesAppendWithSeparator(t *testing.T) {
	in := rlimitRows(100, 200)
	in[4].Notes = "existing"
	got := Apply(pid, fixture(t, "normal"), in)
	if got[4].Notes != "existing; this process has 4 threads" {
		t.Errorf("notes = %q", got[4].Notes)
	}
}

func TestApplyDoesNotMutateInput(t *testing.T) {
	in := rlimitRows(8, 16)
	in[2].Notes = "orig"
	before := make([]model.LimitReading, len(in))
	copy(before, in)
	got := Apply(pid, fixture(t, "over_soft"), in)
	if !reflect.DeepEqual(in, before) {
		t.Errorf("input changed:\n got %+v\nwant %+v", in, before)
	}
	if reflect.DeepEqual(got, before) {
		t.Error("output should differ from input")
	}
}

func TestNoMatchingRowsReadsNothing(t *testing.T) {
	in := []model.LimitReading{row("rlimit.cpu.soft", model.KindSoft, model.KnownAmount(1))}
	got := Apply(pid, errFS{errors.New("must not be read")}, in)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("got %+v", got)
	}
	if got := Apply(pid, fstest.MapFS{}, nil); len(got) != 0 {
		t.Errorf("nil input gave %v", got)
	}
}

// strictFS fails the test on any Open other than the fd directory itself and
// the status file: entries are listed, never opened.
type strictFS struct {
	t *testing.T
	fs.FS
}

func (s strictFS) Open(name string) (fs.File, error) {
	if name != "proc/4242/fd" && name != "proc/4242/status" {
		s.t.Errorf("unexpected Open(%q)", name)
	}
	return s.FS.Open(name)
}

func TestEntriesAreOnlyListed(t *testing.T) {
	got := Apply(pid, strictFS{t: t, FS: mapFS("0", "1", "2")}, rlimitRows(100, 200))
	if got[0].Usage != model.KnownAmount(3) {
		t.Errorf("usage = %+v", got[0].Usage)
	}
}

func TestNprocWithoutPidsRowChangesNothingElse(t *testing.T) {
	in := []model.LimitReading{
		row("rlimit.cpu.soft", model.KindSoft, model.KnownAmount(1)),
		row("rlimit.nproc.soft", model.KindSoft, model.KnownAmount(100)),
	}
	got := Apply(pid, fixture(t, "normal"), in)
	if !reflect.DeepEqual(got[0], in[0]) {
		t.Errorf("unrelated row changed: %+v", got[0])
	}
	if got[1].Notes == "" {
		t.Error("nproc row should carry a note")
	}
}

func TestFailedRowsGetNoNotes(t *testing.T) {
	for _, st := range []model.Status{model.StatusUnparseable, model.StatusUnreadable} {
		t.Run(string(st), func(t *testing.T) {
			in := []model.LimitReading{
				row("rlimit.nproc.soft", model.KindSoft, model.UnknownAmount()),
				row("cgroup.pids.max", model.KindSingle, model.UnknownAmount()),
			}
			in[0].Status, in[1].Status = st, st
			got := Apply(pid, fixture(t, "normal"), in)
			if !reflect.DeepEqual(got, in) {
				t.Errorf("rows changed:\n got %+v\nwant %+v", got, in)
			}
		})
	}
}
