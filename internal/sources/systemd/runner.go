package systemd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner asks systemd about one unit. It is the only way this package talks to
// systemd, and the seam tests replace with a fake that replays captured
// output.
type Runner interface {
	// Show returns the stdout of `systemctl show --no-pager -p <props> -- <unit>`
	// run with LC_ALL=C. props are property names and are joined with commas.
	Show(unit string, props []string) (string, error)
}

const (
	// defaultTimeout bounds one systemctl call, so a wedged manager cannot
	// hang the report. The worst case for a call is this plus waitDelay.
	defaultTimeout = 10 * time.Second
	// maxOutput bounds how much stdout is kept: the answer is a few dozen
	// short lines.
	maxOutput = 1 << 20
	// maxStderr bounds the stderr text quoted in an error.
	maxStderr = 256
	// waitDelay is how long to wait for the pipes after the process is killed.
	waitDelay = 2 * time.Second
)

// ExecRunner is the real Runner. It runs systemctl, and nothing else.
type ExecRunner struct {
	// Path is the systemctl binary; empty means "systemctl" looked up on PATH.
	// Tests point it at a stand-in script.
	Path string
	// Timeout bounds one call; zero means 10 seconds.
	Timeout time.Duration
}

// Show implements [Runner]. A call can take up to Timeout plus waitDelay
// (12 seconds by default): the timeout kills systemctl, and the wait delay is
// how long the pipes may stay open after that. The unit name and every property name are
// validated before anything is executed.
func (e ExecRunner) Show(unit string, props []string) (string, error) {
	if err := ValidateUnitName(unit); err != nil {
		return "", err
	}
	if len(props) == 0 {
		return "", errors.New("no properties requested")
	}
	for _, p := range props {
		if !validProperty(p) {
			return "", fmt.Errorf("invalid property name %q", p)
		}
	}
	path := e.Path
	if path == "" {
		path = "systemctl"
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// The program is fixed and the unit name and properties were validated
	// above; "--" stops systemctl from reading the name as an option.
	cmd := exec.CommandContext(ctx, path, "show", "--no-pager", //nolint:gosec // validated above
		"-p", strings.Join(props, ","), "--", unit)
	// Later duplicates win in os/exec, so this overrides any inherited LC_ALL.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.WaitDelay = waitDelay
	stdout := &cappedBuffer{limit: maxOutput}
	stderr := &cappedBuffer{limit: maxStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("systemctl show timed out after %s: %w", timeout, ctx.Err())
	}
	if err != nil {
		return "", fmt.Errorf("systemctl show: %w%s", err, stderrSuffix(stderr))
	}
	if stdout.truncated {
		return "", fmt.Errorf("systemctl show printed more than %d bytes", maxOutput)
	}
	return stdout.buf.String(), nil
}

func stderrSuffix(cb *cappedBuffer) string {
	text := strings.TrimSpace(cb.buf.String())
	if text == "" {
		return ""
	}
	return ": " + quoteBounded(text)
}

// validProperty accepts the letters and digits of a systemd property name.
func validProperty(p string) bool {
	if p == "" {
		return false
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		isLetterOrDigit := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if !isLetterOrDigit {
			return false
		}
	}
	return true
}

// cappedBuffer keeps at most limit bytes and remembers that it dropped some.
// It always reports the full write as accepted so the child is never blocked
// on a full pipe.
type cappedBuffer struct {
	limit     int
	buf       bytes.Buffer
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room < len(p) {
		c.truncated = true
		if room > 0 {
			_, _ = c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}
