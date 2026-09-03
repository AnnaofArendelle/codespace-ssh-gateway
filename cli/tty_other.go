//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package cli

import "os"

// isTerminal falls back to the character-device heuristic on platforms without
// a termios ioctl.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
