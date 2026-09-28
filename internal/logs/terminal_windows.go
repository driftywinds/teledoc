//go:build windows

package logs

import (
	"os"

	"golang.org/x/sys/windows"
)

// isTerminal reports whether f refers to a console and, on Windows, also
// best-effort enables Virtual Terminal Processing so ANSI SGR sequences
// render in the legacy conhost (cmd.exe) as well as Windows Terminal. The
// VT enable is the reason this file is Windows-specific; on any modern
// terminal it is already on and this is a no-op. Failure is fine: the
// caller's color decision has a plain-text fallback either way.
//
// It takes the caller's *os.File and never wraps f.Fd() in a new *os.File
// (os.NewFile): that second file would own stdout's handle and its GC
// finalizer would CloseHandle it, silently killing all console output some
// seconds after startup.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	// Best-effort VT enable; ignore errors (console APIs fail for
	// redirected/piped handles, which are already excluded above).
	var mode uint32
	h := windows.Handle(f.Fd())
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return true // a console we cannot query: assume ANSI-capable
	}
	const enableVirtualTerminalProcessing = 0x0004
	_ = windows.SetConsoleMode(h, mode|enableVirtualTerminalProcessing)
	return true
}