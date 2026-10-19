package framerate

import (
	"testing"
	"time"
)

func TestDecimateExactRate(t *testing.T) {
	for _, tc := range []struct {
		source  Rate
		request int
		want    Rate
		divisor int
	}{
		{New(60000, 1001), 30, New(30000, 1001), 2},
		{New(120000, 1001), 30, New(30000, 1001), 4},
		{New(60000, 1001), 20, New(20000, 1001), 3},
		{New(5994, 100), 30, New(2997, 100), 2},
		{New(60, 1), 50, New(60, 1), 1},
		{New(30000, 1001), 60, New(30000, 1001), 1},
		{New(599, 10), 30, New(599, 10), 1},
		{Rate{}, 30, Rate{}, 1},
	} {
		got, divisor := tc.source.Decimate(tc.request)
		if got != tc.want || divisor != tc.divisor {
			t.Fatalf("%v -> %d: got %v / %d, want %v / %d", tc.source, tc.request, got, divisor, tc.want, tc.divisor)
		}
	}
}
func TestIntervalAndNormalization(t *testing.T) {
	r := New(60000, 2002)
	if r != (Rate{30000, 1001}) || r.Round() != 30 || r.Interval() != 33366666*time.Nanosecond {
		t.Fatalf("unexpected rate: %v", r)
	}
	if New(-1, 1).Valid() || New(30, 0).Valid() {
		t.Fatal("invalid rate accepted")
	}
	if (Rate{}).Or(30) != New(30, 1) {
		t.Fatal("missing integer fallback")
	}
}
