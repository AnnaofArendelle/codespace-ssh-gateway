//go:build linux

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// isTerminal reports whether f is a real terminal. A character device is not
// enough: /dev/null is one too, and setup must not start from a script.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}
