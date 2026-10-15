//go:build race

package gc_test

// raceEnabled scales down large tests, which run at full size in the
// non-race pass of make test.
const raceEnabled = true
