//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func processGroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return 0
	}
	return pgid
}

func terminateProcess(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}

func terminateProcessGroup(pgid int) error {
	return syscall.Kill(-pgid, syscall.SIGTERM)
}

func killProcessGroup(pgid int) error {
	return syscall.Kill(-pgid, syscall.SIGKILL)
}

func bindingGroupAlive(pgid int) bool { return pgid > 0 && syscall.Kill(-pgid, 0) != syscall.ESRCH }
