package systemd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeSystemctl writes a shell script standing in for systemctl and returns an
// ExecRunner pointing at it. The script runs body with the real arguments.
func fakeSystemctl(t *testing.T, body string) ExecRunner {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "systemctl")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return ExecRunner{Path: path, Timeout: 5 * time.Second}
}

func TestExecRunnerPassesExactArgumentsAndEnvironment(t *testing.T) {
	r := fakeSystemctl(t, `printf '%s\n' "$#" "$@" "LC_ALL=$LC_ALL"`)
	t.Setenv("LC_ALL", "de_DE.UTF-8") // must be overridden, not inherited
	out, err := r.Show("a@b.service", []string{"Id", "LoadState", "LimitNOFILESoft"})
	if err != nil {
		t.Fatal(err)
	}
	want := "6\nshow\n--no-pager\n-p\nId,LoadState,LimitNOFILESoft\n--\na@b.service\nLC_ALL=C\n"
	if out != want {
		t.Errorf("output\n  got  %q\n  want %q", out, want)
	}
}

func TestExecRunnerNeverRunsAnythingForABadRequest(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	r := fakeSystemctl(t, `touch '`+marker+`'`)
	cases := []struct {
		unit  string
		props []string
	}{
		{"", []string{"Id"}},
		{"-x", []string{"Id"}},
		{"foo bar", []string{"Id"}},
		{"a.service", nil},
		{"a.service", []string{"Id;rm"}},
		{"a.service", []string{""}},
		{"a.service", []string{"Id,LoadState"}},
		{"a.service", []string{"--all"}},
	}
	for _, tc := range cases {
		if _, err := r.Show(tc.unit, tc.props); err == nil {
			t.Errorf("Show(%q, %q) = nil error", tc.unit, tc.props)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the command ran for a request that should have been refused")
	}
}

func TestExecRunnerFailures(t *testing.T) {
	tests := []struct {
		name string
		body string
		r    func(ExecRunner) ExecRunner
		want string
	}{
		{"non-zero exit", `echo 'Failed to connect to bus' >&2; exit 1`, nil,
			`exit status 1: "Failed to connect to bus"`},
		{"timeout", `exec sleep 5`, func(r ExecRunner) ExecRunner { r.Timeout = 200 * time.Millisecond; return r },
			"timed out after 200ms"},
		{"too much output", `head -c 2000000 /dev/zero`, nil, "printed more than"},
		{"missing binary", ``, func(r ExecRunner) ExecRunner { r.Path = filepath.Join(t.TempDir(), "nope"); return r },
			"systemctl show"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := fakeSystemctl(t, tc.body)
			if tc.r != nil {
				r = tc.r(r)
			}
			start := time.Now()
			_, err := r.Show("a.service", []string{"Id"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if d := time.Since(start); d > 4*time.Second {
				t.Errorf("took %s", d)
			}
		})
	}
}

func TestExecRunnerDefaults(t *testing.T) {
	// With no Path, the program is looked up as "systemctl" on PATH; make it
	// resolve to a stand-in so nothing real is run.
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\necho Id=x\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	out, err := ExecRunner{}.Show("x.service", []string{"Id"})
	if err != nil || out != "Id=x\n" {
		t.Errorf("got %q, %v", out, err)
	}
}
