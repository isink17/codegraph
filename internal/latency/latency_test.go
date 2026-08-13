package latency

import (
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestPercentileNearestRank(t *testing.T) {
	// Deliberately unsorted: Percentile must not depend on input order.
	s := Samples{ms(5), ms(1), ms(4), ms(2), ms(3), ms(10), ms(6), ms(7), ms(9), ms(8)}
	cases := []struct {
		p    float64
		want time.Duration
	}{
		{0, ms(1)},
		{10, ms(1)},
		{50, ms(5)},
		{90, ms(9)},
		{95, ms(10)},
		{100, ms(10)},
	}
	for _, tc := range cases {
		if got := s.Percentile(tc.p); got != tc.want {
			t.Errorf("Percentile(%v) = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func TestPercentileIsAlwaysAnObservedSample(t *testing.T) {
	s := Samples{ms(1), ms(100)}
	observed := map[time.Duration]bool{ms(1): true, ms(100): true}
	for p := 0; p <= 100; p += 5 {
		got := s.Percentile(float64(p))
		if !observed[got] {
			t.Fatalf("Percentile(%d) = %v, which is not an observed sample", p, got)
		}
	}
}

func TestPercentileSingleAndEmpty(t *testing.T) {
	if got := (Samples{}).Percentile(95); got != 0 {
		t.Errorf("empty Percentile = %v, want 0", got)
	}
	if got := (Samples{}).Max(); got != 0 {
		t.Errorf("empty Max = %v, want 0", got)
	}
	one := Samples{ms(7)}
	for _, p := range []float64{0, 50, 95, 100} {
		if got := one.Percentile(p); got != ms(7) {
			t.Errorf("single Percentile(%v) = %v, want %v", p, got, ms(7))
		}
	}
}

func TestSortedDoesNotMutateReceiver(t *testing.T) {
	s := Samples{ms(3), ms(1), ms(2)}
	_ = s.Sorted()
	if s[0] != ms(3) {
		t.Fatalf("Sorted mutated receiver: %v", s)
	}
}

func TestMax(t *testing.T) {
	if got := (Samples{ms(3), ms(9), ms(1)}).Max(); got != ms(9) {
		t.Errorf("Max = %v, want %v", got, ms(9))
	}
}

func TestMillisRounding(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want float64
	}{
		{0, 0},
		{1500 * time.Nanosecond, 0.002}, // 0.0015ms -> 2us -> 0.002ms
		{time.Millisecond, 1},
		{1234567 * time.Nanosecond, 1.235},
	}
	for _, tc := range cases {
		if got := Millis(tc.in); got != tc.want {
			t.Errorf("Millis(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
