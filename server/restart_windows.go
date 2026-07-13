//go:build windows

package server

import (
	"os/exec"
	"syscall"
)

func prepareRestartCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x00000008, // DETACHED_PROCESS
	}
}
