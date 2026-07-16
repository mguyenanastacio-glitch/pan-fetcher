//go:build !windows

package server

import (
	"os"
	"os/exec"
	"syscall"
)

// selfReplace atomically replaces the running binary (works while running on Linux/macOS).
// Returns (true, nil) — caller should restart.
func selfReplace(newPath, targetPath string) (bool, error) {
	if err := os.Rename(newPath, targetPath); err != nil {
		return false, err
	}
	return true, nil // true = caller should restart
}

func prepareRestartCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Setsid:  true,
	}
}
