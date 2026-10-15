package registry

import "testing"

func TestAll(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("All() returned no sources")
	}
	seen := map[string]bool{}
	for _, s := range all {
		if s.Name() == "" {
			t.Errorf("source %T has an empty name", s)
		}
		if seen[s.Name()] {
			t.Errorf("duplicate source name %q", s.Name())
		}
		seen[s.Name()] = true
	}
	if !seen["rlimit"] {
		t.Error(`the "rlimit" source is not registered`)
	}
}
