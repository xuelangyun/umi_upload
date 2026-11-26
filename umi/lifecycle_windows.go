//go:build windows

package umi

import (
	"os"
	"syscall"
	"time"
)

var winCtrlHandlerRef uintptr

// installPlatformHandlers registers Windows console control handlers.
func installPlatformHandlers() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("SetConsoleCtrlHandler")
	if err := proc.Find(); err != nil {
		return
	}
	winCtrlHandlerRef = syscall.NewCallback(func(ctrlType uint32) uintptr {
		switch ctrlType {
		case 0, 1: // CTRL_C_EVENT, CTRL_BREAK_EVENT
			// These events do not have the close-event timeout. Finish cleanup
			// asynchronously, then exit after all tracked work and SQLite close.
			go func() {
				Cleanup()
				CloseDB()
				os.Exit(0)
			}()
			return 1

		case 2, 5, 6: // CTRL_CLOSE_EVENT, CTRL_LOGOFF_EVENT, CTRL_SHUTDOWN_EVENT
			// Windows terminates the process after this handler returns, even
			// when it returns TRUE. Give cleanup a bounded window below the
			// default 5-second close timeout; work exceeding it cannot be saved.
			done := make(chan struct{})
			go func() {
				Cleanup()
				CloseDB()
				close(done)
			}()
			timer := time.NewTimer(4 * time.Second)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
			}
			return 1

		default:
			return 0
		}
	})
	_, _, _ = proc.Call(winCtrlHandlerRef, 1)
}

func platformShutdownSignals() []os.Signal {
	return nil
}
