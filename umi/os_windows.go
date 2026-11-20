//go:build windows

package umi

import (
	"os/exec"
	"syscall"
)

func OpenInFileManager(p string, isDir bool) error {
	var cmd *exec.Cmd
	if isDir {
		cmd = exec.Command("explorer", p)
	} else {
		cmd = exec.Command("explorer", "/select,"+p)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

func OpenBrowserURL(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}
