//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// maybeRelaunchInTerminal starts a terminal emulator only when the executable
// was opened from a graphical session without an existing terminal. When the
// terminal starts this process again, the TTY check prevents a relaunch loop.
func maybeRelaunchInTerminal() bool {
	if hasLinuxTerminal() || !hasLinuxDesktopSession() {
		return false
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "无法定位程序路径，继续直接启动：%v\n", err)
		return false
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return false
	}

	wd, _ := os.Getwd()
	terminalNames := []string{
		"x-terminal-emulator",
		"gnome-terminal",
		"kgx",
		"konsole",
		"xfce4-terminal",
		"mate-terminal",
		"xterm",
	}
	for _, name := range terminalNames {
		path, lookErr := exec.LookPath(name)
		if lookErr != nil {
			continue
		}
		args, ok := linuxTerminalArgs(name, exe, os.Args[1:])
		if !ok {
			continue
		}
		cmd := exec.Command(path, args...)
		if wd != "" {
			cmd.Dir = wd
		}
		if err := cmd.Start(); err == nil {
			_ = cmd.Process.Release()
			return true
		}
	}

	fmt.Fprintln(os.Stderr, "未找到可用的终端模拟器，请先打开终端并运行本程序。")
	return false
}

func hasLinuxTerminal() bool {
	for _, fd := range []int{0, 1, 2} {
		if _, err := unix.IoctlGetTermios(fd, unix.TCGETS); err == nil {
			return true
		}
	}
	return false
}

func hasLinuxDesktopSession() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// linuxTerminalArgs returns argument vectors without shell interpolation, so
// executable paths and user-supplied flags remain intact even when they contain
// spaces or shell metacharacters.
func linuxTerminalArgs(name, executable string, appArgs []string) ([]string, bool) {
	args := make([]string, 0, len(appArgs)+3)
	switch name {
	case "gnome-terminal", "mate-terminal":
		args = append(args, "--wait", "--", executable)
	case "kgx":
		args = append(args, "--", executable)
	case "konsole":
		args = append(args, "--separate", "-e", executable)
	case "xfce4-terminal":
		args = append(args, "--disable-server", "--execute", executable)
	case "x-terminal-emulator", "xterm":
		args = append(args, "-e", executable)
	default:
		return nil, false
	}
	args = append(args, appArgs...)
	return args, true
}
