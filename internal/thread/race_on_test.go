//go:build race

package thread

// raceEnabled: the race detector slows code down several times, so timing
// bounds are relaxed under it.
const raceEnabled = true
