//go:build !unix && !windows

package platform

// ProcessAlive cannot answer on a platform with neither signals nor a
// process API here, and says so rather than guessing. See the unix
// implementation for what the two return values mean.
func ProcessAlive(int) (alive, known bool) { return false, false }
