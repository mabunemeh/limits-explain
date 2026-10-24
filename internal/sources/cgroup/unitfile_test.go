package cgroup

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestReadFileLimit(t *testing.T) {
	m := fstest.MapFS{
		"sys/fs/cgroup/a/b/pids.max":    {Data: []byte("7\n")},
		"sys/fs/cgroup/a/b/memory.max":  {Data: []byte("max\n")},
		"sys/fs/cgroup/a/b/memory.high": {Data: []byte("lots\n")},
		"sys/fs/cgroup/a/b/cpu.max":     {Data: []byte("3750 50000\n")},
		"sys/fs/cgroup/a/cpu.max":       {Data: []byte("max 100000\n")},
	}
	tests := []struct {
		name string
		path string
		file string
		want FileLimit
	}{
		{"finite scalar", "/a/b", "pids.max", FileLimit{Value: 7}},
		{"unlimited scalar", "/a/b", "memory.max", FileLimit{Unlimited: true}},
		{"cpu keeps the raw pair", "/a/b", "cpu.max", FileLimit{Value: 3750, Period: 50000}},
		{"cpu max", "/a", "cpu.max", FileLimit{Unlimited: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadFileLimit(m, tc.path, tc.file)
			if err != nil || got != tc.want {
				t.Errorf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}

	errs := []struct {
		name string
		path string
		file string
	}{
		{"unparseable", "/a/b", "memory.high"},
		{"unknown file", "/a/b", "cpu.stat"},
		{"relative path", "a/b", "pids.max"},
		{"dot-dot", "/a/../b", "pids.max"},
	}
	for _, tc := range errs {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ReadFileLimit(m, tc.path, tc.file); err == nil {
				t.Errorf("got %+v, want an error", got)
			}
		})
	}

	// A file that is not there wraps ErrNotExist, so a caller can tell absence
	// from a bad value.
	_, err := ReadFileLimit(m, "/a", "pids.max")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want it to wrap fs.ErrNotExist", err)
	}
}
