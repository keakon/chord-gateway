//go:build windows

package gateway

import "os"

func openPairingTerminal() (*os.File, error) { return os.OpenFile("CONOUT$", os.O_WRONLY, 0) }

func readPairingConfirmation(_ *os.File) (string, error) {
	file, err := os.OpenFile("CONIN$", os.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return readConfirmationFromFile(file)
}
