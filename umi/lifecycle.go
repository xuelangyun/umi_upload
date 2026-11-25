package umi

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type workTracker struct {
	mu      sync.Mutex
	stopped bool
	wg      sync.WaitGroup
}

// Begin admits one long-running operation. Stop and Begin share the same lock,
// so Wait can never race with a later WaitGroup.Add.
func (t *workTracker) Begin() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return false
	}
	t.wg.Add(1)
	return true
}

func (t *workTracker) End() {
	t.wg.Done()
}

func (t *workTracker) Stop() {
	t.mu.Lock()
	t.stopped = true
	t.mu.Unlock()
}

func (t *workTracker) Wait() {
	t.wg.Wait()
}

var (
	cleanupOnce sync.Once
	onShutdown  func()
	shutdownMu  sync.Mutex
	activeWork  workTracker

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
		// Close task admission before cancellation. Every admitted scan, move,
		// and job is tracked until its goroutines and persistence work finish.
		activeWork.Stop()
		if AppCancel != nil {
			AppCancel()
		}
		// Wait for any admission already in progress to finish registering its
		// run in Jobs before collecting IDs to mark interrupted.
		StartMu.Lock()
		PL.Lock()
		var runIDs []int64
		for _, j := range Jobs {
			if !j.Done && j.RunID > 0 {
				runIDs = append(runIDs, j.RunID)
			}
		}
		PL.Unlock()
		StartMu.Unlock()

		if len(runIDs) > 0 {
			if db, err := OpenDB(); err == nil {
				for _, id := range runIDs {
					_, _ = db.Exec("UPDATE runs SET status='interrupted', finished_at=? WHERE id=? AND status='running'", NowTS(), id)
				}
			}
		}

		// Workers may still be unwinding canceled SDK calls or writing their
		// final per-dataset records. Close SQLite only after all admitted work exits.
		activeWork.Wait()

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
