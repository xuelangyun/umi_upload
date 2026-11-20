//go:build windows

package umi

import (
	"syscall"
)

var winCtrlHandlerRef uintptr

// installPlatformHandlers registers Windows console event handler (including clicking the ✕ close button on cmd window).
func installPlatformHandlers() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("SetConsoleCtrlHandler")
	if err := proc.Find(); err != nil {
		return
	}
	winCtrlHandlerRef = syscall.NewCallback(func(ctrlType uint32) uintptr {
		// 0=CTRL_C, 1=CTRL_BREAK, 2=CTRL_CLOSE, 5=CTRL_LOGOFF, 6=CTRL_SHUTDOWN
		Cleanup()
		return 0
	})
	_, _, _ = proc.Call(winCtrlHandlerRef, 1)
}
