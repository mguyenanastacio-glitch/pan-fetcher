//go:build windows

package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// selfReplace on Windows writes a batch script that swaps the binary
// and restarts the process after the current one exits.
// Returns (true, nil) — caller should exit; the script handles restart.
func selfReplace(newPath, targetPath string) (bool, error) {
	dir := filepath.Dir(targetPath)
	script := filepath.Join(dir, "update.bat")

	args := ""
	for i, a := range os.Args {
		if i == 0 {
			continue
		}
		args += " " + a
	}

	bat := fmt.Sprintf(
		"@echo off\r\n"+
			":loop\r\n"+
			"timeout /t 1 /nobreak >nul\r\n"+
			"del /f \"%s\" 2>nul\r\n"+
			"if exist \"%s\" goto loop\r\n"+
			"move /y \"%s\" \"%s\"\r\n"+
			"start \"\" \"%s\" %s\r\n"+
			"del \"%%~f0\"\r\n",
		targetPath, targetPath,
		newPath, targetPath,
		targetPath, args,
	)

	if err := os.WriteFile(script, []byte(bat), 0644); err != nil {
		return false, fmt.Errorf("write script: %w", err)
	}

	cmd := exec.Command("cmd", "/c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("start script: %w", err)
	}
	return false, nil // false = caller should NOT restart (script handles it)
}

func prepareRestartCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x00000008, // DETACHED_PROCESS
	}
}
