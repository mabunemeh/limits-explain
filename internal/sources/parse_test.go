package sources

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"testing"
)

func TestDescribe(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"path error drops the prefix", &fs.PathError{Op: "open", Path: "proc/1/limits", Err: fs.ErrPermission}, "permission denied"},
		{"wrapped path error", fmt.Errorf("outer: %w", &fs.PathError{Op: "open", Path: "x", Err: fs.ErrNotExist}), "file does not exist"},
		{"bare error", errors.New("boom"), "boom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Describe(tc.err); got != tc.want {
				t.Errorf("Describe = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsASCIIDigits(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"0", true},
		{"18446744073709551616", true},
		{"+1", false},
		{"-1", false},
		{"", false},
		{"12a", false},
		{"1_0", false},
		{" 1", false},
		{"١٢", false}, // Arabic-Indic digits are digits to unicode, not to the kernel
		{"１２", false}, // fullwidth digits
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%q", tc.in), func(t *testing.T) {
			if got := IsASCIIDigits(tc.in); got != tc.want {
				t.Errorf("IsASCIIDigits(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"LF", "a\nb\n", []string{"a", "b", ""}},
		{"CRLF", "a\r\nb\r\n", []string{"a", "b", ""}},
		{"no trailing newline", "a\nb", []string{"a", "b"}},
		{"empty string", "", []string{""}},
		{"mixed endings", "a\r\nb\nc", []string{"a", "b", "c"}},
		{"interior CR is kept", "a\rb\n", []string{"a\rb", ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SplitLines(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("SplitLines(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
