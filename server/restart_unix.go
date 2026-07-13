//go:build !windows

package server

import (
	"os/exec"
	"syscall"
)

func prepareRestartCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Setsid:  true,
	}
}
