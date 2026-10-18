package sources

import (
	"errors"
	"io/fs"
	"strings"
)

// Describe returns the innermost error text for a read error, without the
// "open proc/1/limits:" prefix that *fs.PathError adds, since every
// SourceError reports the path separately.
func Describe(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// IsASCIIDigits reports whether s is a non-empty run of ASCII digits.
//
// strconv.ParseUint with base 10 is already this strict on its own: unlike
// ParseInt it permits no sign at all, and it rejects underscores, hex and
// float forms and non-ASCII digits. This check is defence in depth. It
// states the intent where the value is parsed, and it keeps the strictness
// independent of the base argument: base 0 would accept "0x400" and
// "10_240" and would read a leading zero as octal, turning "0000050" into
// 40. Sources call it before ParseUint so every source agrees on what "a
// number" is.
func IsASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// SplitLines splits text into lines, accepting both LF and CRLF endings.
//
// strings.Split on "\n" leaves the "\r" of a CRLF ending on the end of every
// line. Left in place it would end up inside a unit string or a number that
// then fails to parse. A trailing newline yields a final empty element,
// which callers treat like any other blank line. The result always has at
// least one element.
func SplitLines(text string) []string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	return lines
}
