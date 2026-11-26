//go:build !linux

package main

func maybeRelaunchInTerminal() bool {
	return false
}
