package umi

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var (
	cleanupOnce sync.Once
	onShutdown  func()
	shutdownMu  sync.Mutex

	// ShutdownLaterFunc can be overridden in tests to observe `/api/shutdown` without exiting the test binary.
	ShutdownLaterFunc = defaultShutdownLater
)

func SetShutdownCallback(fn func()) {
	shutdownMu.Lock()
	onShutdown = fn
	shutdownMu.Unlock()
}

// Cleanup cancels in-flight SDK operations and marks any running DB run as interrupted.
func Cleanup() {
	cleanupOnce.Do(func() {
		if AppCancel != nil {
			AppCancel()
		}
		PL.Lock()
		var runIDs []int64
		for _, j := range Jobs {
			if !j.Done && j.RunID > 0 {
				runIDs = append(runIDs, j.RunID)
			}
		}
		PL.Unlock()

		if len(runIDs) > 0 {
			if db, err := OpenDB(); err == nil {
				for _, id := range runIDs {
					_, _ = db.Exec("UPDATE runs SET status='interrupted', finished_at=? WHERE id=? AND status='running'", NowTS(), id)
				}
			}
		}

		shutdownMu.Lock()
		cb := onShutdown
		shutdownMu.Unlock()
		if cb != nil {
			cb()
		}
	})
}

func defaultShutdownLater() {
	go func() {
		time.Sleep(250 * time.Millisecond)
		Cleanup()
		CloseDB()
		os.Exit(0)
	}()
}

// InstallHandlers registers SIGINT/SIGTERM and Windows console close handlers.
func InstallHandlers() {
	installPlatformHandlers()
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		Cleanup()
		CloseDB()
		os.Exit(0)
	}()
}
