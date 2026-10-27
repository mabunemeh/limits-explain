//go:build integration && linux

package integration

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// TestCLIMeasuresOwnOpenFiles runs the CLI on the test process against the
// real root. The process always holds stdin, stdout and stderr, so a measured
// rlimit.nofile.soft must show at least three descriptors. Exit 1 is allowed:
// a busy host can legitimately have another reading over --warn.
func TestCLIMeasuresOwnOpenFiles(t *testing.T) {
	code, stdout, stderr := show("show", strconv.Itoa(os.Getpid()), "--json")
	if code != 0 && code != 1 {
		t.Fatalf("exit %d, stderr: %s\n%s", code, stderr, stdout)
	}
	var rows []struct {
		Key    string  `json:"key"`
		Status string  `json:"status"`
		Usage  *uint64 `json:"usage"`
	}
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, stdout)
	}
	for _, r := range rows {
		if r.Key != "rlimit.nofile.soft" {
			continue
		}
		if r.Status != "ok" || r.Usage == nil || *r.Usage < 3 {
			t.Fatalf("rlimit.nofile.soft = status %q usage %v, want ok and >= 3", r.Status, r.Usage)
		}
		return
	}
	t.Fatal("no rlimit.nofile.soft row in the report")
}
