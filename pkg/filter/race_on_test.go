//go:build race

package filter

// raceSlowdown scales time bounds under the race detector, which runs this code about 20 times slower.
const raceSlowdown = 20
