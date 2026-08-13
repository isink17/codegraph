//go:build !race

package store

// raceEnabled reports whether the binary was built with the race detector.
const raceEnabled = false
