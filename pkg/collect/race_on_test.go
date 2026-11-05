//go:build race

package collect

// raceEnabled: sync.Pool drops items under the race detector, so regexp allocates a machine per search.
const raceEnabled = true
