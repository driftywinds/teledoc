//go:build !windows

package logs

import (
	"os"
)

// isTerminal reports whether f refers to a character device (a terminal).
// Redirects, pipes and docker log capture are not terminals and get no
// colors.
//
// It takes the caller's *os.File and only Stat()s it. It must NOT build a
// second *os.File from f.Fd() (os.NewFile): that new file owns the same
// descriptor and its finalizer closes it when the GC runs, which silently
// kills stdout for the whole process some time after startup.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}