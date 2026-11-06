//go:build race

package filter

// raceSlowdown divides the filter count of the timed property test: the race detector runs this code about 20 times slower.
const raceSlowdown = 20
