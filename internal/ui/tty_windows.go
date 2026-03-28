//go:build windows

package ui

import (
	"os"
	"syscall"
	"unsafe"
)

const enableVirtualTerminalProcessing = 0x0004

// isTTY reports whether stdout is a real Windows console and, if so, enables
// ANSI/VT processing so that escape codes render correctly.
func isTTY() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")

	handle := syscall.Handle(os.Stdout.Fd())
	var mode uint32
	r, _, _ := getConsoleMode.Call(uintptr(handle), uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		return false // stdout is not a console (pipe / redirect)
	}

	// Enable VT/ANSI escape sequence processing (Windows 10+).
	setConsoleMode.Call(uintptr(handle), uintptr(mode|enableVirtualTerminalProcessing))
	return true
}
