//go:build windows

package umi

import (
	"fmt"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	shell32DLL     = syscall.NewLazyDLL("shell32.dll")
	shellExecuteW  = shell32DLL.NewProc("ShellExecuteW")
)

// OpenInFileManager opens a directory with the Windows Shell. ShellExecuteW is
// used instead of starting explorer.exe directly: Explorer may hand the request
// to an existing process and exit immediately, so cmd.Start() alone is not a
// reliable indication that the folder-open request was accepted.
func OpenInFileManager(p string, isDir bool) error {
	verb, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return fmt.Errorf("转换操作名称失败：%w", err)
	}

	file, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return fmt.Errorf("转换目录路径失败：%w", err)
	}

	var params *uint16
	if !isDir {
		file, err = syscall.UTF16PtrFromString("explorer.exe")
		if err != nil {
			return fmt.Errorf("转换资源管理器程序路径失败：%w", err)
		}
		params, err = syscall.UTF16PtrFromString(fmt.Sprintf("/select,\\"%s\\"", p))
		if err != nil {
			return fmt.Errorf("转换选中文件参数失败：%w", err)
		}
	}

	if err := shell32DLL.Load(); err != nil {
		return fmt.Errorf("加载 Windows Shell 失败：%w", err)
	}
	if err := shellExecuteW.Find(); err != nil {
		return fmt.Errorf("加载 ShellExecuteW 失败：%w", err)
	}

	var param uintptr
	if params != nil {
		param = uintptr(unsafe.Pointer(params))
	}
	result, _, _ := shellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		param,
		0,
		1, // SW_SHOWNORMAL
	)
	runtime.KeepAlive(verb)
	runtime.KeepAlive(file)
	runtime.KeepAlive(params)

	// ShellExecuteW returns a value greater than 32 on success. Values at or
	// below 32 are documented shell error codes (not GetLastError values).
	if result <= 32 {
		return fmt.Errorf("Windows Shell 打开路径失败，错误码：%d（路径：%s）", result, p)
	}
	return nil
}

func OpenBrowserURL(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}
