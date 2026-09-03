//go:build linux

package github

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openPTY allocates a pseudo-terminal pair. The exec connector needs one
// because `gh codespace ssh` runs the real ssh client, which only behaves
// interactively when its stdin is a terminal.
func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("unlock pty: %w", err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("get pty number: %w", err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("open pty slave: %w", err)
	}
	return m, s, nil
}

// setWinsize applies a window size to the pty.
func setWinsize(f *os.File, rows, cols uint16) error {
	if f == nil {
		return fmt.Errorf("no pty")
	}
	return unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ,
		&unix.Winsize{Row: rows, Col: cols})
}

// ptyProcAttr makes the child a session leader with the pty as its controlling
// terminal (Ctty 0 == the child's stdin, which is the pty slave).
func ptyProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Setctty: true}
}

func ptySupported() bool { return true }
