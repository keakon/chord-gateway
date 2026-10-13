//go:build !windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func takeInstanceLock(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errInstanceRunning
	}
	return err
}
func releaseInstanceLock(f *os.File) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
