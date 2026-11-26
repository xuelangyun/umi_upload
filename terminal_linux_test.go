//go:build linux

package main

import (
	"reflect"
	"testing"
)

func TestLinuxTerminalArgsPreserveExecutableAndArguments(t *testing.T) {
	appArgs := []string{"--port", "8765", "--db", "/tmp/a b/umi_web.db"}
	tests := []struct {
		name string
		want []string
	}{
		{"x-terminal-emulator", []string{"-e", "/opt/UMI App/umi_upload-linux-amd64", "--port", "8765", "--db", "/tmp/a b/umi_web.db"}},
		{"xterm", []string{"-e", "/opt/UMI App/umi_upload-linux-amd64", "--port", "8765", "--db", "/tmp/a b/umi_web.db"}},
		{"gnome-terminal", []string{"--wait", "--", "/opt/UMI App/umi_upload-linux-amd64", "--port", "8765", "--db", "/tmp/a b/umi_web.db"}},
		{"mate-terminal", []string{"--wait", "--", "/opt/UMI App/umi_upload-linux-amd64", "--port", "8765", "--db", "/tmp/a b/umi_web.db"}},
		{"kgx", []string{"--", "/opt/UMI App/umi_upload-linux-amd64", "--port", "8765", "--db", "/tmp/a b/umi_web.db"}},
		{"konsole", []string{"--separate", "-e", "/opt/UMI App/umi_upload-linux-amd64", "--port", "8765", "--db", "/tmp/a b/umi_web.db"}},
		{"xfce4-terminal", []string{"--disable-server", "--execute", "/opt/UMI App/umi_upload-linux-amd64", "--port", "8765", "--db", "/tmp/a b/umi_web.db"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := linuxTerminalArgs(tt.name, "/opt/UMI App/umi_upload-linux-amd64", appArgs)
			if !ok {
				t.Fatal("terminal command was not recognized")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("args = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestLinuxTerminalArgsRejectUnknownTerminal(t *testing.T) {
	if _, ok := linuxTerminalArgs("unknown-terminal", "/app", nil); ok {
		t.Fatal("unknown terminal should not be accepted")
	}
}
