package model

import (
	"encoding/json"
	"math"
	"testing"
)

func reading(limit, usage Amount) LimitReading {
	return LimitReading{
		Key:    "rlimit.nofile.single",
		Kind:   KindSingle,
		Label:  "open files",
		Source: "rlimit",
		Limit:  limit,
		Usage:  usage,
		Unit:   "files",
	}
}

func TestDefaultStatus(t *testing.T) {
	tests := []struct {
		name  string
		limit Amount
		usage Amount
		want  Status
	}{
		// Unlimited outranks unmeasured: there is no ceiling to measure
		// usage against.
		{"unlimited, no usage", UnknownAmount(), UnknownAmount(), StatusUnlimited},
		{"unlimited, usage known", UnknownAmount(), KnownAmount(5), StatusUnlimited},
		{"unmeasured", KnownAmount(1024), UnknownAmount(), StatusUnmeasured},
		{"ok", KnownAmount(1024), KnownAmount(512), StatusOK},
		{"zero limit is a real limit", KnownAmount(0), UnknownAmount(), StatusUnmeasured},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DefaultStatus(tc.limit, tc.usage); got != tc.want {
				t.Errorf("DefaultStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAmountConstructors(t *testing.T) {
	if a := UnknownAmount(); a != (Amount{}) {
		t.Errorf("UnknownAmount() = %+v, want the zero Amount", a)
	}
	if a := KnownAmount(0); !a.Known || a.Value != 0 {
		t.Errorf("KnownAmount(0) = %+v, want known zero", a)
	}
	// A known zero and an unknown amount must not compare equal; the rlimits
	// soft == hard collapse relies on struct equality.
	if KnownAmount(0) == UnknownAmount() {
		t.Error("KnownAmount(0) == UnknownAmount()")
	}
}

func TestAmountMarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   Amount
		want string
	}{
		{"unknown", UnknownAmount(), "null"},
		{"zero", KnownAmount(0), "0"},
		{"small", KnownAmount(1024), "1024"},
		// RLIM_INFINITY - 1 overflows int64 and exceeds 2^53.
		{"above int64", KnownAmount(18446744073709551614), "18446744073709551614"},
		{"max uint64", KnownAmount(math.MaxUint64), "18446744073709551615"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestLimitReadingJSONTags(t *testing.T) {
	r := LimitReading{
		Key: "k", Kind: KindSoft, Label: "l", Source: "s",
		Limit: KnownAmount(7), Usage: UnknownAmount(), Unit: "u",
		Status: StatusUnmeasured, Notes: "n",
	}
	got, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"key":"k","kind":"soft","label":"l","source":"s","limit":7,"usage":null,` +
		`"unit":"u","status":"unmeasured","notes":"n"}`
	if string(got) != want {
		t.Errorf("Marshal = %s\nwant      %s", got, want)
	}
}
