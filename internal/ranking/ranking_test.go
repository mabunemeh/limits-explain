package ranking

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/mabunemeh/limits-explain/internal/model"
)

// r builds a reading keyed by key. A nil limit means unknown, a nil usage
// means unknown; status is derived the way a source would.
func r(key string, limit, usage *uint64) model.LimitReading {
	l, u := model.UnknownAmount(), model.UnknownAmount()
	if limit != nil {
		l = model.KnownAmount(*limit)
	}
	if usage != nil {
		u = model.KnownAmount(*usage)
	}
	return model.LimitReading{Key: key, Limit: l, Usage: u, Status: model.DefaultStatus(l, u)}
}

func p(v uint64) *uint64 { return &v }

func gap(key string, status model.Status) model.LimitReading {
	return model.LimitReading{Key: key, Status: status}
}

func keys(rs []model.LimitReading) []string {
	out := make([]string, 0, len(rs))
	for _, x := range rs {
		out = append(out, x.Key)
	}
	return out
}

func TestRankOrder(t *testing.T) {
	const pow60 = uint64(1) << 60
	tests := []struct {
		name string
		in   []model.LimitReading
		want []string
	}{
		{"empty", nil, []string{}},
		{"measured by fraction descending", []model.LimitReading{
			r("a", p(100), p(10)), r("b", p(100), p(90)), r("c", p(100), p(50)),
		}, []string{"b", "c", "a"}},
		{"over the limit ranks above 100 percent", []model.LimitReading{
			r("full", p(100), p(100)), r("over", p(100), p(112)), r("low", p(100), p(1)),
		}, []string{"over", "full", "low"}},
		{"measured, then gaps, then unlimited", []model.LimitReading{
			r("unlimited", nil, nil),
			gap("src.source_error", model.StatusUnreadable),
			r("unmeasured", p(100), nil),
			r("measured", p(100), p(1)),
			gap("unparseable", model.StatusUnparseable),
		}, []string{"measured", "src.source_error", "unmeasured", "unparseable", "unlimited"}},
		{"a measured 0 percent still outranks a gap", []model.LimitReading{
			r("unmeasured", p(100), nil), r("zero", p(100), p(0)),
		}, []string{"zero", "unmeasured"}},
		// float64 reads usage 2^60 on limit 2^60+1 as exactly 1.0, a tie with
		// a full reading that stable order would then get wrong.
		{"2^60 near-tie is ordered exactly", []model.LimitReading{
			r("almost", p(pow60+1), p(pow60)), r("full", p(pow60), p(pow60)),
		}, []string{"full", "almost"}},
		{"2^60 near-tie in the other collection order", []model.LimitReading{
			r("full", p(pow60), p(pow60)), r("almost", p(pow60+1), p(pow60)),
		}, []string{"full", "almost"}},
		{"zero limit is fully consumed", []model.LimitReading{
			r("partial", p(100), p(99)), r("zero", p(0), p(0)),
		}, []string{"zero", "partial"}},
		{"ties keep collection order", []model.LimitReading{
			r("t1", p(100), p(50)), r("t2", p(1000), p(500)), r("t3", p(10), p(5)), r("hi", p(10), p(9)),
		}, []string{"hi", "t1", "t2", "t3"}},
		{"gap ties keep collection order", []model.LimitReading{
			gap("g1", model.StatusUnreadable), gap("g2", model.StatusUnparseable), gap("g3", model.StatusUnreadable),
		}, []string{"g1", "g2", "g3"}},
		{"unlimited ties keep collection order", []model.LimitReading{
			r("u1", nil, nil), r("u2", nil, nil),
		}, []string{"u1", "u2"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := keys(Rank(tc.in)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Rank = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRankDoesNotMutateItsInput(t *testing.T) {
	in := []model.LimitReading{r("a", p(100), p(1)), r("b", p(100), p(99))}
	_ = Rank(in)
	if got := keys(in); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("input order changed to %v", got)
	}
}

func TestRankIsDeterministic(t *testing.T) {
	in := []model.LimitReading{
		r("a", p(10), p(5)), r("b", p(20), p(10)), gap("c", model.StatusUnreadable),
		r("d", nil, nil), r("e", p(10), p(5)),
	}
	first := keys(Rank(in))
	for range 20 {
		if got := keys(Rank(in)); !reflect.DeepEqual(got, first) {
			t.Fatalf("Rank varied: %v vs %v", got, first)
		}
	}
	if want := []string{"a", "b", "e", "c", "d"}; !reflect.DeepEqual(first, want) {
		t.Errorf("Rank = %v, want %v", first, want)
	}
}

func TestSummarize(t *testing.T) {
	tests := []struct {
		name        string
		in          []model.LimitReading
		warn        uint64
		total       int
		nearest     string
		notMeasured int
		warned      int
	}{
		{"empty", nil, 90, 0, "", 0, 0},
		{"all unlimited", []model.LimitReading{r("u", nil, nil)}, 90, 1, "", 0, 0},
		{"only gaps", []model.LimitReading{
			gap("g", model.StatusUnreadable), r("m", p(5), nil), r("u", nil, nil),
		}, 90, 3, "", 2, 0},
		{"measured under the threshold", []model.LimitReading{
			r("a", p(100), p(62)), gap("g", model.StatusUnreadable), r("u", nil, nil),
		}, 90, 3, "a", 1, 0},
		{"exactly at the threshold warns", []model.LimitReading{r("a", p(10), p(9))}, 90, 1, "a", 0, 1},
		{"just under the threshold does not", []model.LimitReading{r("a", p(10000000), p(8999999))}, 90, 1, "a", 0, 0},
		{"nearest is the highest fraction, first on a tie", []model.LimitReading{
			r("a", p(100), p(50)), r("b", p(10), p(5)), r("c", p(100), p(60)), r("d", p(10), p(6)),
		}, 90, 4, "c", 0, 0},
		{"counts every reading at or above", []model.LimitReading{
			r("a", p(100), p(95)), r("b", p(100), p(100)), r("c", p(100), p(120)), r("d", p(100), p(10)),
		}, 90, 4, "c", 0, 3},
		{"warned regardless of gaps", []model.LimitReading{
			gap("g", model.StatusUnparseable), r("a", p(100), p(100)),
		}, 90, 2, "a", 1, 1},
		{"zero limit warns at any threshold", []model.LimitReading{r("z", p(0), p(0))}, 100, 1, "z", 0, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := Summarize(tc.in, tc.warn)
			if s.Total != tc.total || s.NotMeasured != tc.notMeasured || s.Warned != tc.warned {
				t.Errorf("Summary = %+v, want total %d notMeasured %d warned %d",
					s, tc.total, tc.notMeasured, tc.warned)
			}
			if tc.nearest == "" {
				if s.HasNearest {
					t.Errorf("HasNearest with nearest %q, want none", s.Nearest.Key)
				}
			} else if !s.HasNearest || s.Nearest.Key != tc.nearest {
				t.Errorf("Nearest = %q (has %v), want %q", s.Nearest.Key, s.HasNearest, tc.nearest)
			}
		})
	}
}

func TestSummarizeDoesNotNeedRankedInput(t *testing.T) {
	in := []model.LimitReading{r("low", p(100), p(1)), r("high", p(100), p(80))}
	if got := Summarize(in, 90).Nearest.Key; got != "high" {
		t.Errorf("Nearest = %q, want high", got)
	}
}

// TestRankIsStableAtScale uses far more than 12 rows with many ties. Go's
// sort.Slice falls back to insertion sort (which happens to be stable) below
// 12 elements, so small inputs cannot tell sort.Slice from sort.SliceStable;
// this one can.
func TestRankIsStableAtScale(t *testing.T) {
	const n = 60
	var in []model.LimitReading
	byUsage := map[uint64][]string{}
	var gaps, unlimited []string
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("r%02d", i)
		switch i % 6 {
		case 0:
			in = append(in, r(key, nil, nil))
			unlimited = append(unlimited, key)
		case 1:
			in = append(in, gap(key, model.StatusUnreadable))
			gaps = append(gaps, key)
		default:
			// Ten distinct fractions, so each is shared by several rows;
			// scales of 100 and 1000 give equal fractions with different
			// limits.
			usage := uint64((i*7)%10) * 10
			if i%2 == 0 {
				in = append(in, r(key, p(100), p(usage)))
			} else {
				in = append(in, r(key, p(1000), p(usage*10)))
			}
			byUsage[usage] = append(byUsage[usage], key)
		}
	}
	var want []string
	for usage := uint64(90); ; usage -= 10 {
		want = append(want, byUsage[usage]...)
		if usage == 0 {
			break
		}
	}
	want = append(append(want, gaps...), unlimited...)
	if got := keys(Rank(in)); !reflect.DeepEqual(got, want) {
		t.Errorf("Rank order not stable:\n got %v\nwant %v", got, want)
	}
}
