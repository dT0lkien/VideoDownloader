//go:build !windows

// Заглушки для разработки на macOS/Linux: окна нет, страницу открываем в браузере сами.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func singleInstance() bool { return true }

func runWindow(url, data string) bool { return false }

func videosDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Movies")
}

func clipboardText() string {
	out, _ := exec.Command("pbpaste").Output()
	return string(out)
}

func openPath(p string) { exec.Command("open", p).Start() }

func hide(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func track(cmd *exec.Cmd) {}

func killTree(cmd *exec.Cmd) { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
