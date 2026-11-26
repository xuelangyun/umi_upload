//go:build !windows && !linux

package umi

import "os"

func installPlatformHandlers() {}

func platformShutdownSignals() []os.Signal {
	return nil
}
