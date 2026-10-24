// Package systemd annotates the readings of other sources with what systemd
// was told to configure for the unit the process belongs to.
//
// It adds NO rows. A directive that lands in another layer (TasksMax is
// pids.max, MemoryMax is memory.max, Limit<X> is an rlimit) becomes a note on
// the cgroup or rlimit row it set, saying what systemd configured and, when
// the kernel holds something else, what is effective. Ranking, statuses,
// limits, usage and gap counts are never touched.
//
// The unit is found from the process's own cgroup path, and its directives are
// read by running `systemctl show` through an injected [Runner]. No systemd, or
// a process outside a system unit, is not an error: there is nothing to
// annotate and the table says why on its "unit:" line. A systemctl that is
// present but fails, prints garbage, or reports a unit that is not loaded IS
// an error, because the directives could not be trusted.
//
// Scope: system units only. A process under user@N.service, and directives
// set on ancestor slices, are out of scope.
package systemd

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/mabunemeh/limits-explain/internal/model"
	"github.com/mabunemeh/limits-explain/internal/sources"
)

// Name is the value used for the source in source_error rows.
const Name = "systemd"

// Reasons a process has no unit. They are shown on the table's "unit:" line.
const (
	// ReasonNotConsulted is used when systemd is deliberately not asked
	// because the CLI was pointed at another root.
	ReasonNotConsulted = "not consulted under --root"

	reasonPID1Unreadable = "PID 1 name unreadable"
	reasonPID1NotSystemd = "PID 1 is not systemd"
	reasonCgroupRead     = "cgroup of the process unreadable"
	reasonNoUnified      = "no unified cgroup entry"
	reasonNotInUnit      = "not in a systemd unit"
	reasonUserManager    = "user manager unit, not read"
	reasonBadUnitName    = "cgroup unit name is not valid"
)

// maxUnitName is systemd's limit on a unit name in bytes.
const maxUnitName = 255

// ValidateUnitName rejects a name before it can reach systemctl: non-empty, at
// most 255 bytes, only [A-Za-z0-9:_.@-] and backslash, and never starting with
// '-' (which systemctl would read as an option). systemctl itself escapes a
// bad name and exits 0, so this check is the only guard.
func ValidateUnitName(name string) error {
	switch {
	case name == "":
		return errors.New("unit name must not be empty")
	case len(name) > maxUnitName:
		return fmt.Errorf("unit name is longer than %d bytes", maxUnitName)
	case name == "." || name == "..":
		return fmt.Errorf("invalid unit name %s", quoteBounded(name))
	case name[0] == '-':
		return fmt.Errorf("invalid unit name %s: must not start with '-'", quoteBounded(name))
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == ':' || c == '_' || c == '.' || c == '@' || c == '-' || c == '\\'
		if !ok {
			return fmt.Errorf("invalid unit name %s: allowed characters are letters, digits and : _ . @ - \\",
				quoteBounded(name))
		}
	}
	return nil
}

// rlimitProps maps each systemd Limit<X> property to the rlimit key segment
// the rlimits source uses for the same limit.
var rlimitProps = []struct{ name, segment string }{
	{"CPU", "cpu"}, {"FSIZE", "fsize"}, {"DATA", "data"}, {"STACK", "stack"},
	{"CORE", "core"}, {"RSS", "rss"}, {"NOFILE", "nofile"}, {"AS", "as"},
	{"NPROC", "nproc"}, {"MEMLOCK", "memlock"}, {"LOCKS", "locks"},
	{"SIGPENDING", "sigpending"}, {"MSGQUEUE", "msgqueue"}, {"NICE", "nice"},
	{"RTPRIO", "rtprio"}, {"RTTIME", "rttime"},
}

// properties lists every property asked of systemctl.
func properties() []string {
	props := []string{
		"Id", "LoadState", "ActiveState", "MainPID", "ControlGroup",
		"TasksMax", "MemoryMax", "MemoryHigh", "CPUQuotaPerSecUSec", "CPUQuotaPeriodUSec",
	}
	for _, l := range rlimitProps {
		props = append(props, "Limit"+l.name, "Limit"+l.name+"Soft")
	}
	return props
}

// Unit is the parsed answer of `systemctl show` for one unit.
type Unit struct {
	// ID is the unit's canonical name as systemd reports it.
	ID          string
	LoadState   string
	ActiveState string
	// MainPID is 0 when the unit has none or does not report one (scopes
	// have no MainPID line at all).
	MainPID int
	// ControlGroup is the unit's cgroup path, such as "/system.slice/a.service".
	ControlGroup string

	tasksMax, memoryMax, memoryHigh, cpuQuota directive
	cpuPeriod                                 string
	limits                                    map[string]directive
}

// Loaded reports whether systemd has the unit loaded. A unit that does not
// exist is NOT an error to systemctl: it exits 0 and prints default values, so
// nothing in the output may be trusted unless this is true.
func (u *Unit) Loaded() bool { return u.LoadState == "loaded" }

func newUnit(props map[string]string) (*Unit, error) {
	state, ok := props["LoadState"]
	if !ok {
		return nil, errors.New("LoadState is missing from the output")
	}
	u := &Unit{
		ID:           props["Id"],
		LoadState:    state,
		ActiveState:  props["ActiveState"],
		ControlGroup: props["ControlGroup"],
		limits:       map[string]directive{},
	}
	if raw, ok := props["MainPID"]; ok {
		if !sources.IsASCIIDigits(raw) {
			return nil, fmt.Errorf("MainPID is not a number: %s", quoteBounded(raw))
		}
		v, err := strconv.ParseUint(raw, 10, 31)
		if err != nil {
			return nil, fmt.Errorf("MainPID is out of range: %s", quoteBounded(raw))
		}
		u.MainPID = int(v)
	}
	number := func(name string) directive {
		raw, ok := props[name]
		if !ok {
			return directive{}
		}
		return parseNumber(raw)
	}
	u.tasksMax = number("TasksMax")
	u.memoryMax = number("MemoryMax")
	u.memoryHigh = number("MemoryHigh")
	if raw, ok := props["CPUQuotaPerSecUSec"]; ok {
		u.cpuQuota = parseQuota(raw)
	}
	u.cpuPeriod = props["CPUQuotaPeriodUSec"]
	for _, l := range rlimitProps {
		for _, name := range []string{"Limit" + l.name, "Limit" + l.name + "Soft"} {
			if d := number(name); d.state != stateAbsent {
				u.limits[name] = d
			}
		}
	}
	return u, nil
}

// Query runs `systemctl show` for unit through r and parses it. A failing
// runner is an unreadable [sources.SourceError]; output that is not
// Key=Value lines is an unparseable one. Query does not check LoadState:
// callers decide what a unit that is not loaded means.
func Query(r Runner, unit string) (*Unit, error) {
	path := "systemctl show " + unit
	out, err := r.Show(unit, properties())
	if err != nil {
		return nil, &sources.SourceError{Path: path, Detail: "failed: " + err.Error(), Err: err}
	}
	props, err := parseShow(out)
	if err != nil {
		return nil, &sources.SourceError{Path: path, Detail: "unparseable output: " + err.Error()}
	}
	u, err := newUnit(props)
	if err != nil {
		return nil, &sources.SourceError{Path: path, Detail: "unparseable output: " + err.Error()}
	}
	return u, nil
}

// Outcome says what the systemd step did, for the table's context line.
type Outcome struct {
	// Unit is the unit the process belongs to, or empty.
	Unit string
	// Reason says why there is no unit; set only when Unit is empty.
	Reason string
	// Failed is true when the unit was found but its directives could not be
	// read, so the report carries a systemd.source_error row.
	Failed bool
}

// Line is the one-line context shown under the headline.
func (o Outcome) Line() string {
	switch {
	case o.Unit != "" && o.Failed:
		return "unit: " + o.Unit + " (directives not read)"
	case o.Unit != "":
		return "unit: " + o.Unit
	}
	return "unit: none (" + o.Reason + ")"
}

// pid1Reason returns why PID 1 cannot be taken for systemd, or "" when it is.
func pid1Reason(fsys fs.FS) string {
	comm, err := fs.ReadFile(fsys, "proc/1/comm")
	switch {
	case err != nil:
		return reasonPID1Unreadable
	case strings.TrimSpace(string(comm)) != "systemd":
		return reasonPID1NotSystemd
	}
	return ""
}

// resolution is the outcome of looking for the unit of a pid.
type resolution struct {
	unit   string // empty when there is no unit
	cgroup string // the pid's cgroup path, set when unit is
	reason string // why there is no unit
}

// resolve finds the system unit of pid from proc/<pid>/cgroup: the ROOTMOST
// path component ending in .service or .scope. Rootmost matters because a
// service may start sub-services or scopes below itself, and user@N.service
// owns everything beneath it. Every failure to look is a reason, never an
// error.
func resolve(pid int, fsys fs.FS) resolution {
	if reason := pid1Reason(fsys); reason != "" {
		return resolution{reason: reason}
	}
	data, err := fs.ReadFile(fsys, fmt.Sprintf("proc/%d/cgroup", pid))
	if err != nil {
		return resolution{reason: reasonCgroupRead}
	}
	var unified []string
	for _, line := range sources.SplitLines(string(data)) {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			unified = append(unified, rest)
		}
	}
	if len(unified) != 1 {
		return resolution{reason: reasonNoUnified}
	}
	comps, ok := splitCgroup(unified[0])
	if !ok {
		return resolution{reason: reasonNoUnified}
	}
	for _, c := range comps {
		suffix := ""
		switch {
		case strings.HasSuffix(c, ".service"):
			suffix = ".service"
		case strings.HasSuffix(c, ".scope"):
			suffix = ".scope"
		default:
			continue
		}
		if len(c) == len(suffix) {
			continue // no name before the suffix
		}
		if isUserManager(c) {
			return resolution{reason: reasonUserManager}
		}
		if ValidateUnitName(c) != nil {
			return resolution{reason: reasonBadUnitName}
		}
		return resolution{unit: c, cgroup: unified[0]}
	}
	return resolution{reason: reasonNotInUnit}
}

// isUserManager reports whether name is user@<digits>.service.
func isUserManager(name string) bool {
	id, ok := strings.CutPrefix(name, "user@")
	if !ok {
		return false
	}
	id, ok = strings.CutSuffix(id, ".service")
	return ok && sources.IsASCIIDigits(id)
}

// splitCgroup splits an absolute cgroup path into its components, refusing a
// relative path and the components "." and "..".
func splitCgroup(p string) ([]string, bool) {
	if !strings.HasPrefix(p, "/") {
		return nil, false
	}
	var comps []string
	for _, c := range strings.Split(p, "/") {
		switch c {
		case "":
		case ".", "..":
			return nil, false
		default:
			comps = append(comps, c)
		}
	}
	return comps, true
}

// ownsCgroup reports whether controlGroup is the pid's cgroup or an ancestor
// of it, compared component by component.
func ownsCgroup(controlGroup, pidCgroup string) bool {
	cg, ok := splitCgroup(controlGroup)
	if !ok || len(cg) == 0 {
		return false
	}
	pc, ok := splitCgroup(pidCgroup)
	if !ok || len(cg) > len(pc) {
		return false
	}
	for i := range cg {
		if cg[i] != pc[i] {
			return false
		}
	}
	return true
}

// Consult is the whole systemd step for one pid. readings are the other
// sources' readings; the result is the same readings with notes added, plus
// the outcome for the context line. r == nil means systemd is not consulted.
//
// No systemd, or no system unit, returns the readings unchanged and a nil
// error. Anything that stops the directives from being trusted returns the
// readings unchanged and a [*sources.SourceError]: systemctl failing, a
// timeout, garbage output, a unit that is not loaded or is not the one asked
// for, or a ControlGroup that does not contain the process.
func Consult(r Runner, pid int, fsys fs.FS, readings []model.LimitReading) ([]model.LimitReading, Outcome, error) {
	if r == nil {
		return readings, Outcome{Reason: ReasonNotConsulted}, nil
	}
	res := resolve(pid, fsys)
	if res.unit == "" {
		return readings, Outcome{Reason: res.reason}, nil
	}
	failed := Outcome{Unit: res.unit, Failed: true}
	path := "systemctl show " + res.unit
	u, err := Query(r, res.unit)
	if err != nil {
		return readings, failed, err
	}
	switch {
	case !u.Loaded():
		return readings, failed, &sources.SourceError{Path: path, Detail: fmt.Sprintf(
			"LoadState=%s, expected loaded; no directive is trusted", quoteBounded(u.LoadState))}
	case u.ID != res.unit:
		return readings, failed, &sources.SourceError{Path: path, Detail: fmt.Sprintf(
			"Id=%s does not match the unit asked for", quoteBounded(u.ID))}
	case !ownsCgroup(u.ControlGroup, res.cgroup):
		return readings, failed, &sources.SourceError{Path: path, Detail: fmt.Sprintf(
			"ControlGroup=%s is neither the process cgroup %s nor an ancestor of it",
			quoteBounded(u.ControlGroup), quoteBounded(res.cgroup))}
	}
	return Annotate(readings, u, fsys), Outcome{Unit: u.ID}, nil
}

// MainPID resolves a unit name to its main process for `show --unit`. name
// must already have passed [ValidateUnitName]. Every failure is a plain error
// whose text is the message for the user: no systemd, a unit that is not
// loaded, a unit with no main process.
func MainPID(r Runner, fsys fs.FS, name string) (int, error) {
	if r == nil {
		return 0, errors.New("systemd is " + ReasonNotConsulted)
	}
	if reason := pid1Reason(fsys); reason != "" {
		return 0, fmt.Errorf("systemd is not available: %s", reason)
	}
	u, err := Query(r, name)
	if err != nil {
		return 0, err
	}
	switch {
	case !u.Loaded():
		return 0, fmt.Errorf("unit %s not found", name)
	case u.MainPID == 0:
		return 0, fmt.Errorf("unit %s has no main process", name)
	}
	return u.MainPID, nil
}
