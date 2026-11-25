//go:build windows

package umi

import (
	"os"
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
		// Do not block the Windows callback: CTRL_CLOSE handlers have a short
		// OS-enforced deadline. Cleanup itself tracks and waits for active work.
		go func() {
			Cleanup()
			CloseDB()
			os.Exit(0)
		}()
		// TRUE tells Windows this handler accepted the event. The process exits
		// after Cleanup and DB close complete, subject to Windows' close deadline.
		return 1
	})
	_, _, _ = proc.Call(winCtrlHandlerRef, 1)
}
