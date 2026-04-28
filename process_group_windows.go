//go:build windows

package main

import (
	"os"
	"os/exec"
)

func configureProcessGroup(cmd *exec.Cmd) {
}

func processGroupID(cmd *exec.Cmd) int {
	return 0
}

func terminateProcess(proc *os.Process) error {
	return proc.Signal(os.Interrupt)
}

func terminateProcessGroup(pgid int) error {
	return nil
}

func killProcessGroup(pgid int) error {
	return nil
}
