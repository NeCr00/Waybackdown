//go:build !windows

package ui

import "os"

// isTTY reports whether stdout is an interactive terminal on Unix/Linux/macOS.
// Respects the NO_COLOR (https://no-color.org) and TERM=dumb conventions.
func isTTY() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
