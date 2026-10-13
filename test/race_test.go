//go:build race

package test

// raceEnabled scales down large transfers, which run at full size in the
// non-race pass of make test.
const raceEnabled = true
