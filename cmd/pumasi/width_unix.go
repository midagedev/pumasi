//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// ttyWidth is the terminal's columns when out is one, else 0.
func ttyWidth(f *os.File) int {
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(ws.Col)
}
