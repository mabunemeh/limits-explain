//go:build integration && linux

package integration

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// TestCLIMeasuresOwnOpenFiles runs the CLI on the test process against the
// real root. The process always holds stdin, stdout and stderr, so the soft
// open-files row must be measured with at least three descriptors. That row
// is rlimit.nofile.single when soft equals hard, as on GitHub's runners, and
// rlimit.nofile.soft otherwise. Exit 1 is allowed: a busy host can
// legitimately have another reading over --warn.
func TestCLIMeasuresOwnOpenFiles(t *testing.T) {
	code, stdout, stderr := show("show", strconv.Itoa(os.Getpid()), "--json")
	if code != 0 && code != 1 {
		t.Fatalf("exit %d, stderr: %s\n%s", code, stderr, stdout)
	}
	var env struct {
		Readings []struct {
			Key    string  `json:"key"`
			Status string  `json:"status"`
			Usage  *uint64 `json:"usage"`
		} `json:"readings"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not the JSON envelope: %v\n%s", err, stdout)
	}
	for _, r := range env.Readings {
		if r.Key != "rlimit.nofile.soft" && r.Key != "rlimit.nofile.single" {
			continue
		}
		if r.Status != "ok" || r.Usage == nil || *r.Usage < 3 {
			t.Fatalf("%s = status %q usage %v, want ok and >= 3", r.Key, r.Status, r.Usage)
		}
		return
	}
	t.Fatal("no rlimit.nofile.soft or rlimit.nofile.single row in the report")
}
