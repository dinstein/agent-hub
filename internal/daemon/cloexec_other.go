//go:build !unix

package daemon

import "os"

// Nothing to do off unix: no descriptor arrives here in the first place —
// os/exec cannot pass one on Windows — so there is none to keep out of a
// downstream child. The function exists so LifelineFromFD stays one
// implementation rather than two.
func markCloseOnExec(*os.File) {}
