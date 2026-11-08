//go:build race

package integration

// The race job looks for data races; the 100 rounds of the acceptance
// criterion run in the regular test job, without the detector's slowdown.
const raceEnabled = true
