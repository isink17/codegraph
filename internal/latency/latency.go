// Package latency holds the percentile arithmetic shared by the query
// benchmark command and the in-repo benchmark suites.
//
// It is a leaf package on purpose. The store's own benchmarks live in
// `package store`, so anything they share with `internal/querybench` (which
// imports the store) has to sit below both of them or the test build cycles.
package latency

import (
	"math"
	"sort"
	"time"
)

// Samples is a set of measured durations in arbitrary order.
type Samples []time.Duration

// Sorted returns a sorted copy, leaving the receiver untouched. Callers that
// need several percentiles should sort once and use SortedPercentile.
func (s Samples) Sorted() Samples {
	out := make(Samples, len(s))
	copy(out, s)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Percentile returns the p-th percentile (0..100) using the nearest-rank
// method: the smallest sample at or above rank ceil(p/100 * n).
//
// Nearest-rank rather than interpolation because every reported value is then
// an observation that actually happened. An interpolated "p95" of 4.7ms when
// no run took 4.7ms invites the reader to treat the number as a measurement
// when it is an estimate. It also makes p100 == max by construction.
//
// Returns 0 for an empty sample set.
func (s Samples) Percentile(p float64) time.Duration {
	return s.Sorted().SortedPercentile(p)
}

// SortedPercentile is Percentile for an already-sorted set. Behaviour is
// undefined (not incorrect, just meaningless) if the receiver is not sorted
// ascending.
func (s Samples) SortedPercentile(p float64) time.Duration {
	if len(s) == 0 {
		return 0
	}
	if p <= 0 {
		return s[0]
	}
	if p >= 100 {
		return s[len(s)-1]
	}
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(s) {
		rank = len(s)
	}
	return s[rank-1]
}

// Max returns the largest sample, or 0 when empty.
func (s Samples) Max() time.Duration {
	var out time.Duration
	for _, d := range s {
		if d > out {
			out = d
		}
	}
	return out
}

// Millis converts a duration to milliseconds, rounded to three decimals so
// report output stays stable and compact rather than carrying nanosecond
// noise that differs on every run.
func Millis(d time.Duration) float64 {
	return math.Round(float64(d.Nanoseconds())/1e3) / 1e3
}
