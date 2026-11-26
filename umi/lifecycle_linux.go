//go:build linux

package umi

import (
	"os"
	"syscall"
)

func installPlatformHandlers() {}

func platformShutdownSignals() []os.Signal {
	// Closing the terminal sends SIGHUP to its foreground process group.
	// Handle it through Cleanup so active work and SQLite state are finalized.
	return []os.Signal{syscall.SIGHUP}
}
