//go:build !linux

package github

import (
	"os"
	"syscall"
)

func openPTY() (*os.File, *os.File, error) { return nil, nil, errNoPTY }

func setWinsize(*os.File, uint16, uint16) error { return errNoPTY }

func ptyProcAttr() *syscall.SysProcAttr { return nil }

func ptySupported() bool { return false }
