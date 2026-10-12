//go:build race

package tree

// raceEnabled scales down the large algorithmic tests, which run at full
// size in the non-race pass of make test.
const raceEnabled = true
