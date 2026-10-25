//go:build !windows

package gateway

import "os"

func openPairingTerminal() (*os.File, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) }

func readPairingConfirmation(file *os.File) (string, error) { return readConfirmationFromFile(file) }
