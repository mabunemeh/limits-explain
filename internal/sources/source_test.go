package sources

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"
)

func TestSourceErrorMessage(t *testing.T) {
	err := &SourceError{Path: "proc/4242/limits", Detail: "no data rows"}
	if got, want := err.Error(), "proc/4242/limits: no data rows"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if err.Unwrap() != nil {
		t.Errorf("Unwrap() = %v, want nil for a structural failure", err.Unwrap())
	}
}

func TestSourceErrorWrapsReadErrors(t *testing.T) {
	tests := []struct {
		name   string
		cause  error
		target error
		other  error
	}{
		{"permission", fs.ErrPermission, fs.ErrPermission, fs.ErrNotExist},
		{"absence", fs.ErrNotExist, fs.ErrNotExist, fs.ErrPermission},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var err error = &SourceError{Path: "p", Detail: "unreadable", Err: tc.cause}
			// Wrapping further must not hide the cause either.
			err = fmt.Errorf("collecting: %w", err)

			if !errors.Is(err, tc.target) {
				t.Errorf("errors.Is(err, %v) = false, want true", tc.target)
			}
			if errors.Is(err, tc.other) {
				t.Errorf("errors.Is(err, %v) = true, want false", tc.other)
			}
			var se *SourceError
			if !errors.As(err, &se) || se.Path != "p" {
				t.Errorf("errors.As did not recover the *SourceError: %v", se)
			}
		})
	}
}
