//go:build !windows

package umi

import (
	"os/exec"
	"runtime"
)

func OpenInFileManager(p string, _ bool) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", p).Start()
	}
	return exec.Command("xdg-open", p).Start()
}

func OpenBrowserURL(url string) {
	if runtime.GOOS == "darwin" {
		_ = exec.Command("open", url).Start()
		return
	}
	if path, err := exec.LookPath("xdg-open"); err == nil && path != "" {
		_ = exec.Command(path, url).Start()
	}
}
