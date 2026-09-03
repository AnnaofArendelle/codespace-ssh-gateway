//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// isTerminal reports whether f is a real terminal.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	return err == nil
}
