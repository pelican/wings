//go:build !race

package plugin

// raceDetector is true when the tests run with -race, which changes
// allocation counts.
const raceDetector = false
