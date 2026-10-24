package cgroup

import (
	"fmt"
	"io/fs"
	"strings"
)

// FileLimit is what one controller file says at one level of the hierarchy.
type FileLimit struct {
	// Unlimited is true when the file says "max" (for cpu.max, a quota of
	// "max").
	Unlimited bool
	// Value is the finite limit: the bytes or task count of a scalar
	// controller, or the quota in microseconds of cpu.max. It is 0 when
	// Unlimited.
	Value uint64
	// Period is the cpu.max period in microseconds, and 0 for every other
	// controller and for an unlimited cpu.max.
	Period uint64
}

// ReadFileLimit reads one controller file (memory.max, memory.high, pids.max
// or cpu.max) at exactly one cgroup, with no ancestor walk, using the same
// strict parser as the source. cgroupPath is the absolute cgroup path such as
// "/system.slice/a.service".
//
// It exists so another source can compare a value it was told about against
// what the kernel holds at a specific level, which can differ from the level a
// row displays. A missing file wraps [fs.ErrNotExist]; a read failure wraps
// the cause; content that is not recognised is an error too, never a guess.
func ReadFileLimit(fsys fs.FS, cgroupPath, file string) (FileLimit, error) {
	var c *controller
	for i := range controllers {
		if controllers[i].file == file {
			c = &controllers[i]
		}
	}
	if c == nil {
		return FileLimit{}, fmt.Errorf("cgroup: %q is not a controller file this source reads", file)
	}
	if !strings.HasPrefix(cgroupPath, "/") {
		return FileLimit{}, fmt.Errorf("cgroup path %q is not absolute", cgroupPath)
	}
	chain, err := buildChain("cgroup path", cgroupPath)
	if err != nil {
		return FileLimit{}, fmt.Errorf("cgroup path: %w", err)
	}
	name := chain[0].dir + "/" + c.file
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return FileLimit{}, fmt.Errorf("read %s: %w", name, err)
	}
	p := c.parse(strings.TrimSpace(string(data)))
	switch p.kind {
	case kindUnlimited:
		return FileLimit{Unlimited: true}, nil
	case kindFinite:
		if c.isCPU {
			return FileLimit{Value: p.num, Period: p.den}, nil
		}
		return FileLimit{Value: p.value}, nil
	default:
		return FileLimit{}, fmt.Errorf("%s: unparseable content %s", name, quoteBounded(strings.TrimSpace(string(data))))
	}
}
